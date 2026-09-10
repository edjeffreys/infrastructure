#!/usr/bin/env python3
"""PostToolUse hook: hand every comment line an edit just added back to the agent.

Whether a comment is *necessary* is a judgement no linter can make, so this does
not try. It reports what was added and forces the agent to re-read it against the
repo rule, which is the step that actually gets skipped.

Diff-scoped on purpose: this repo carries a lot of deliberate commentary, and a
file-level density check would flag all of it.
"""

import hashlib
import json
import os
import re
import sys

# Extension -> (line-comment tokens, block-comment open/close pairs).
SYNTAX = {
    ".yaml": (["#"], []), ".yml": (["#"], []), ".py": (["#"], []),
    ".sh": (["#"], []), ".bash": (["#"], []), ".zsh": (["#"], []),
    ".tf": (["#", "//"], [("/*", "*/")]), ".hcl": (["#", "//"], [("/*", "*/")]),
    ".toml": (["#"], []), ".conf": (["#"], []), ".ini": ([";", "#"], []),
    ".go": (["//"], [("/*", "*/")]), ".ts": (["//"], [("/*", "*/")]),
    ".tsx": (["//"], [("/*", "*/")]), ".js": (["//"], [("/*", "*/")]),
    ".jsx": (["//"], [("/*", "*/")]), ".rs": (["//"], [("/*", "*/")]),
    ".c": (["//"], [("/*", "*/")]), ".h": (["//"], [("/*", "*/")]),
}

HEREDOC = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")
REDIRECT = re.compile(r">>?\s*([^\s|&;<>]+)")
TEE = re.compile(r"\btee\s+(?:-a\s+)?([^\s|&;<>]+)")
# Writes whose content is not recoverable from the command text.
OPAQUE = re.compile(
    r"\bsed\s+(?:-[a-zA-Z]*i|--in-place)"
    r"|\b(?:perl|ruby)\s+(?:-[a-zA-Z]*i|-pi)"
    r"|open\([^)]*['\"][wa]"
    r"|\.write_text\(|\.writeText\(|>>?\s*['\"]?\$",
)

# Machine-readable directives that happen to use comment syntax.
DIRECTIVE = re.compile(
    r"^\s*(?:#!|#\s*(?:yamllint|noqa|type:|nosec|renovate|shellcheck|pylint|ruff|fmt:)"
    r"|//\s*(?:nolint|eslint|prettier|@ts-|renovate))",
    re.IGNORECASE,
)


def comment_lines(text, exts):
    """Full-line and trailing comments in `text`, as (stripped, is_full_line)."""
    line_tokens, block_pairs = exts
    out, in_block, closer = [], False, None
    for raw in text.splitlines():
        line = raw.strip()
        if in_block:
            out.append((line, True))
            if closer in line:
                in_block = False
            continue
        opened = False
        for open_tok, close_tok in block_pairs:
            if line.startswith(open_tok):
                out.append((line, True))
                if close_tok not in line[len(open_tok):]:
                    in_block, closer = True, close_tok
                opened = True
                break
        if opened or DIRECTIVE.match(line):
            continue
        for tok in line_tokens:
            if line.startswith(tok):
                out.append((line, True))
                break
            # Trailing comment, ignoring the token inside a quoted string.
            idx = line.find(" " + tok)
            if idx > 0 and line[:idx].count('"') % 2 == 0 and line[:idx].count("'") % 2 == 0:
                out.append((line[idx:].strip(), False))
                break
    return out


def heredocs(command):
    """(header line, body) for each heredoc in a shell command."""
    lines, out, i = command.splitlines(), [], 0
    while i < len(lines):
        m = HEREDOC.search(lines[i])
        if not m:
            i += 1
            continue
        delim, header, body = m.group(2), lines[i], []
        i += 1
        while i < len(lines) and lines[i].strip() != delim:
            body.append(lines[i])
            i += 1
        out.append((header, "\n".join(body)))
        i += 1
    return out


def in_project(path):
    root = os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()
    return os.path.abspath(os.path.join(root, path)).startswith(os.path.abspath(root))


def source_targets(text):
    """Paths in `text` that this repo would treat as source."""
    out = []
    for m in re.finditer(r"[\w./~-]+\.[A-Za-z]+", text):
        p = m.group(0)
        if os.path.splitext(p)[1].lower() in SYNTAX and in_project(p) and p not in out:
            out.append(p)
    return out


def bash_added(command):
    """(path, comments) per source file a shell command writes, plus opaque paths."""
    reports, opaque = [], []
    for header, body in heredocs(command):
        lhs = header.split("<<")[0]
        m = TEE.search(lhs) or REDIRECT.search(lhs)
        target = m.group(1).strip("\"'") if m else None
        ext = os.path.splitext(target or "")[1].lower()
        if target and ext in SYNTAX and in_project(target):
            reports.append((target, comment_lines(body, SYNTAX[ext])))
        elif OPAQUE.search(command):
            opaque.extend(source_targets(command))
    if OPAQUE.search(command) and not heredocs(command):
        opaque.extend(source_targets(command))
    named = {p for p, _ in reports}
    return reports, [p for p in dict.fromkeys(opaque) if p not in named]


def added(payload):
    """Comment lines this tool call introduced, oldest edit first."""
    name = payload.get("tool_name", "")
    inp = payload.get("tool_input", {}) or {}
    ext = os.path.splitext(inp.get("file_path", ""))[1].lower()
    if ext not in SYNTAX:
        return []
    syntax = SYNTAX[ext]

    pairs = []
    if name == "Write":
        pairs = [("", inp.get("content", "") or "")]
    elif name == "Edit":
        pairs = [(inp.get("old_string", "") or "", inp.get("new_string", "") or "")]
    elif name == "MultiEdit":
        pairs = [(e.get("old_string", "") or "", e.get("new_string", "") or "")
                 for e in inp.get("edits", []) or []]

    found = []
    for old, new in pairs:
        before = {c for c, _ in comment_lines(old, syntax)}
        for text, full in comment_lines(new, syntax):
            if text not in before and text not in [f[0] for f in found]:
                found.append((text, full))
    return found


def seen_path(session):
    tmp = os.environ.get("TMPDIR", "/tmp").rstrip("/")
    return f"{tmp}/claude-comment-guard-{session or 'nosession'}.txt"


def main():
    try:
        payload = json.load(sys.stdin)
    except Exception:
        return 0

    inp = payload.get("tool_input", {}) or {}
    opaque = []
    if payload.get("tool_name") == "Bash":
        groups, opaque = bash_added(inp.get("command", "") or "")
    else:
        groups = [(inp.get("file_path", "the file"), added(payload))]

    if not any(c for _, c in groups) and not opaque:
        return 0

    # One report per distinct comment per session: re-editing a nearby line must
    # not re-flag a comment the agent already justified.
    path = seen_path(payload.get("session_id"))
    try:
        with open(path) as fh:
            seen = set(fh.read().split())
    except OSError:
        seen = set()

    def key(text):
        return hashlib.sha1(text.encode()).hexdigest()[:12]

    fresh = [(label, [(t, f) for t, f in found if key(t) not in seen])
             for label, found in groups]
    fresh = [(label, found) for label, found in fresh if found]
    opaque = [p for p in opaque if key("opaque:" + p) not in seen]
    if not fresh and not opaque:
        return 0

    try:
        with open(path, "a") as fh:
            for _, found in fresh:
                for text, _ in found:
                    fh.write(key(text) + "\n")
            for p in opaque:
                fh.write(key("opaque:" + p) + "\n")
    except OSError:
        pass

    total = sum(len(f) for _, f in fresh)
    blocks = []
    for label, found in fresh:
        listing = "\n".join(f"  {t[:120]}" for t, _ in found[:15])
        if len(found) > 15:
            listing += f"\n  ... and {len(found) - 15} more"
        blocks.append(f"{label}:\n{listing}")
    report = "\n".join(blocks)

    if opaque:
        note = (
            "Wrote via a script or in-place edit, so the added comments could not "
            "be read: " + ", ".join(opaque) +
            "\nRe-read what you added to those against the rule below."
        )
        report = f"{report}\n\n{note}" if report else note

    headline = (f"COMMENT CHECK — this edit added {total} comment line(s):"
                if total else "COMMENT CHECK — this edit wrote comments this hook could not read:")

    print(
        f"{headline}\n{report}\n\n"
        "Rule: comment ONLY to explain a non-obvious why — a hidden constraint, "
        "a workaround, a surprising consequence. Never restate what the line below it "
        "already says. Keep any that survive to one or two lines.\n\n"
        "Take each one: delete it, or shorten it. Then state in one line which you "
        "kept and why. Do not re-explain the rule.",
        file=sys.stderr,
    )
    return 2


sys.exit(main())
