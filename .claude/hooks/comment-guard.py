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

    found = added(payload)
    if not found:
        return 0

    # One report per distinct comment per session: re-editing a nearby line must
    # not re-flag a comment the agent already justified.
    path = seen_path(payload.get("session_id"))
    try:
        with open(path) as fh:
            seen = set(fh.read().split())
    except OSError:
        seen = set()

    fresh = [(t, f) for t, f in found
             if hashlib.sha1(t.encode()).hexdigest()[:12] not in seen]
    if not fresh:
        return 0

    try:
        with open(path, "a") as fh:
            for text, _ in fresh:
                fh.write(hashlib.sha1(text.encode()).hexdigest()[:12] + "\n")
    except OSError:
        pass

    listing = "\n".join(f"  {t[:120]}" for t, _ in fresh[:15])
    if len(fresh) > 15:
        listing += f"\n  ... and {len(fresh) - 15} more"

    print(
        f"COMMENT CHECK — this edit added {len(fresh)} comment line(s) to "
        f"{payload.get('tool_input', {}).get('file_path', 'the file')}:\n"
        f"{listing}\n\n"
        "Rule: comment ONLY to explain a non-obvious why — a hidden constraint, "
        "a workaround, a surprising consequence. Never restate what the line below it "
        "already says. Keep any that survive to one or two lines.\n\n"
        "Take each one: delete it, or shorten it. Then state in one line which you "
        "kept and why. Do not re-explain the rule.",
        file=sys.stderr,
    )
    return 2


sys.exit(main())
