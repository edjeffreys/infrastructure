#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# ///
"""Comment on a Renovate PR with what the bump means for this repository.

  1. triage   (jev-latest): risk and merge/review/block, from the change and its notes
  2. locate   (jev-latest): relevance of every manifest in the repo to those notes
  3. comment  (jev-router): the PR comment, reading the notes against the most relevant files

For a Helm chart bump the notes are extended with facts rendered locally: the diff of the
chart's default values, the diff of the manifests the chart renders with this repo's values,
and the application's GitHub releases when the chart's appVersion moves. The rendered diff is
the most trustworthy input -- it is what would actually change in the cluster.

Advisory only: it never approves, merges or blocks. A PR with no notes and no chart diff gets
a fixed "review manually" comment instead of a guess.

DESCRIBE_BUMPS decides which PRs get steps 2 and 3; triage always runs and is cheap:
  none     triage verdict only
  flagged  describe only bumps triage marks review or block (the default)
  all      describe every bump, including the ones that do not affect us

Environment: OPENROUTER_API_KEY, GITHUB_TOKEN, GITHUB_REPOSITORY, PR_NUMBER, and optionally
DESCRIBE_BUMPS and HELM (path to the helm binary). Pass --dry-run to print the comment
instead of posting it.
"""

import difflib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
REPO = os.environ["GITHUB_REPOSITORY"]
HELM = os.environ.get("HELM", "helm")
MARKER = "<!-- review-dependency-bump -->"
MAX_FILES = 6
RELEVANCE_FLOOR = 0.3


def request(url, body=None, method=None, headers=None, attempts=2):
    data = json.dumps(body).encode() if body is not None else None
    for attempt in range(attempts):
        req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=300) as response:
                raw = response.read().decode()
                return json.loads(raw) if "json" in response.headers.get("Content-Type", "") else raw
        except urllib.error.HTTPError as error:
            # jev-router intermittently times out choosing a model; one retry clears it.
            if error.code >= 500 and attempt + 1 < attempts:
                time.sleep(10)
                continue
            sys.exit(f"{method or ('POST' if data else 'GET')} {url}: HTTP {error.code}: "
                     f"{error.read().decode()[:1000]}")


def openrouter(path, body):
    return request("https://openrouter.ai/api" + path, body, headers={
        "Authorization": f"Bearer {os.environ['OPENROUTER_API_KEY']}",
        "Content-Type": "application/json"})


def github(path, body=None, method=None, accept="application/vnd.github+json"):
    return request("https://api.github.com" + path, body, method, headers={
        "Authorization": f"Bearer {os.environ['GITHUB_TOKEN']}",
        "Accept": accept, "X-GitHub-Api-Version": "2022-11-28"})


def pull_request(number):
    meta = github(f"/repos/{REPO}/pulls/{number}")
    diff = github(f"/repos/{REPO}/pulls/{number}", accept="application/vnd.github.diff")
    # Provider lock files churn dozens of checksum lines that say nothing about the upgrade.
    diff = re.sub(r'(?m)^[-+]\s*"(h1|zh):[^"]*",?\n', "", diff)
    body = meta["body"] or ""
    has_notes = "### Release Notes" in body
    notes = body.split("### Configuration")[0]
    notes = re.sub(r"<[^>]+>|&#8203;", "", notes)
    notes = re.sub(r"\n{2,}", "\n", notes).strip()
    return {"title": meta["title"], "diff": diff, "release_notes": notes}, has_notes


def unified(a, b, label_a, label_b):
    return "\n".join(difflib.unified_diff(a.splitlines(), b.splitlines(), label_a, label_b,
                                          n=2, lineterm=""))


def version_key(tag):
    return tuple(int(p) for p in re.findall(r"\d+", tag)[:4])


def our_values_file(helmrelease):
    configmap = re.search(r"valuesFrom:\s*\n\s*- kind: ConfigMap\s*\n\s*name:\s*(\S+)", helmrelease)
    if not configmap:
        return None
    for kustomization in ROOT.glob("kubernetes/*/kustomization.yaml"):
        generator = re.search(
            rf"name:\s*{re.escape(configmap.group(1))}\s*\n\s*files:\s*\n(?:\s*#.*\n)*\s*-\s*(?:[\w.-]+=)?(\S+)",
            kustomization.read_text())
        if generator:
            return kustomization.parent / generator.group(1)
    return None


def app_release_notes(sources, old, new):
    for source in sources:
        repo = re.match(r"https://github.com/([^/]+/[^/#?]+)", source)
        if not repo:
            continue
        try:
            releases = github(f"/repos/{repo.group(1)}/releases?per_page=50")
        except SystemExit:
            continue
        picked = sorted((r for r in releases
                         if not r["prerelease"]
                         and version_key(old) < version_key(r["tag_name"]) <= version_key(new)
                         and len(version_key(r["tag_name"])) == len(version_key(new))),
                        key=lambda r: version_key(r["tag_name"]))
        if picked:
            return "\n".join(f"### {repo.group(1)} {r['tag_name']}\n{r['body'] or ''}" for r in picked)
    return None


def chart_facts(diff):
    path = re.search(r"^\+\+\+ b/(\S+)", diff, re.M)
    chart = re.search(r"^[ +-][ \t]*chart:[ \t]*([\w.-]+)$", diff, re.M)
    old = re.search(r'^-\s*version:\s*"?([^"\s]+)', diff, re.M)
    new = re.search(r'^\+\s*version:\s*"?([^"\s]+)', diff, re.M)
    if not (path and chart and old and new):
        return ""
    chart, old, new = chart.group(1), old.group(1), new.group(1)
    source = (ROOT / path.group(1)).read_text()
    repo_name = re.search(r"sourceRef:\s*\n\s*kind: HelmRepository\s*\n\s*name:\s*(\S+)", source)
    if not repo_name:
        return ""
    repo = None
    for candidate in [ROOT / path.group(1), *ROOT.glob("flux/**/*.yaml")]:
        repo = re.search(rf"(?ms)^kind: HelmRepository$.*?name:\s*{re.escape(repo_name.group(1))}\s*$.*?^\s*url:\s*(\S+)",
                         candidate.read_text())
        if repo:
            break
    if not repo:
        return ""
    helmrelease = source.split("kind: HelmRelease", 1)[-1]
    release = re.search(r"releaseName:\s*(\S+)", helmrelease)
    namespace = re.search(r"namespace:\s*(\S+)", helmrelease)

    def helm(*args, version):
        return subprocess.run([HELM, *args, "--repo", repo.group(1), "--version", version],
                              capture_output=True, text=True, check=True).stdout

    sections = [("Chart default values diff",
                 unified(helm("show", "values", chart, version=old), helm("show", "values", chart, version=new),
                         f"{chart} {old} values.yaml", f"{chart} {new} values.yaml")
                 or "No changes to the chart's default values.")]

    values = our_values_file(helmrelease)
    render = ["template", release.group(1) if release else chart, chart,
              "--namespace", namespace.group(1) if namespace else "default"]
    if values:
        render += ["-f", str(values)]

    def stable_render(version):
        # Charts that generate secrets render differently every time; a line that changes
        # between two renders of the same version is noise, not an upgrade effect.
        first, second = helm(*render, version=version), helm(*render, version=version)
        unstable = set(first.splitlines()) ^ set(second.splitlines())
        kept = [line for line in first.splitlines()
                if line not in unstable
                and not re.match(r"\s*(helm\.sh/chart|chart|app\.kubernetes\.io/version):", line)]
        return "\n".join(kept), len(unstable) // 2

    render_old, noise_old = stable_render(old)
    render_new, noise_new = stable_render(new)
    rendered = unified(render_old, render_new, f"{chart} {old} rendered", f"{chart} {new} rendered")
    if noise_old or noise_new:
        rendered += (f"\n(Omitted {max(noise_old, noise_new)} line(s) that differ between two renders of the "
                     f"same version, e.g. generated passwords and their checksums.)")
    sections.append((f"Rendered manifest diff with this repository's values "
                     f"({values.relative_to(ROOT) if values else 'chart defaults'})",
                     rendered or "The rendered manifests are identical."))

    meta_old, meta_new = helm("show", "chart", chart, version=old), helm("show", "chart", chart, version=new)
    app_old = re.search(r"(?m)^appVersion:\s*\"?([^\"\s]+)", meta_old)
    app_new = re.search(r"(?m)^appVersion:\s*\"?([^\"\s]+)", meta_new)
    if app_old and app_new and app_old.group(1) != app_new.group(1):
        sources = re.findall(r"(?m)^\s*-\s*(https://github.com/\S+)", meta_new) + \
                  re.findall(r"(?m)^home:\s*(https://github.com/\S+)", meta_new)
        notes = app_release_notes(sources, app_old.group(1), app_new.group(1))
        sections.append((f"Application version {app_old.group(1)} -> {app_new.group(1)}",
                         notes or "Release notes for the application could not be found."))
    else:
        sections.append(("Application version", f"Unchanged ({app_new.group(1) if app_new else 'unknown'})."))

    return "".join(f"\n\n## {title}\n{body}" for title, body in sections)


def triage(change):
    questions = {
        "risk": {"type": "score",
                 "instructions": "How likely is this dependency bump to need attention beyond merging: breaking changes, required manual steps, or changed defaults?",
                 "criteria": ["0: routine; nothing in the notes needs attention",
                              "1: almost certainly needs manual attention or will break something"]},
        "action": {"type": "choice",
                   "instructions": "Based on the change and its release/upgrade notes, what should happen to this pull request?",
                   "criteria": {"merge": "Routine; merge without further review.",
                                "review": "Something in the notes may affect a deployment and should be checked against the repository's configuration.",
                                "block": "The notes make clear this cannot be merged as-is."}},
    }
    return openrouter("/alpha/decisions", {"model": "~typesafe/jev-latest", "state": change,
                                           "questions": questions})


def summarise(path):
    text = path.read_text(errors="replace")
    resources = []
    for doc in re.split(r"(?m)^---\s*$", text):
        kind = re.search(r"(?m)^kind:\s*(\S+)", doc)
        name = re.search(r"(?m)^  name:\s*(\S+)", doc)
        if kind:
            resources.append(f"{kind.group(1)}/{name.group(1) if name else '?'}")
    keys = re.findall(r"(?m)^([A-Za-z][\w.-]*):", text) if not resources else []
    return ", ".join(resources) or ("top-level keys: " + ", ".join(keys[:15]))


def locate(change):
    files = subprocess.run(["git", "ls-files", "flux/*.yaml", "kubernetes/*.yaml", "talos/*.yaml"],
                           cwd=ROOT, capture_output=True, text=True, check=True).stdout.split()
    ids = {f"file_{i}": path for i, path in enumerate(files)}
    state = {"change": {"title": change["title"], "diff": change["diff"]},
             "notes": change["release_notes"],
             "repository_files": {fid: f"{path}: {summarise(ROOT / path)}" for fid, path in ids.items()}}
    question = lambda path: {
        "type": "score",
        "instructions": f"How relevant is repository file {path} to checking whether anything in the notes affects this repository? Relevant files configure the upgraded dependency, set values the notes mention, or define resources whose behaviour the notes change.",
        "criteria": ["0: unrelated to the upgraded dependency and to everything the notes mention",
                     "1: must be read to judge whether the notes affect this repository"]}
    result = openrouter("/alpha/decisions", {"model": "~typesafe/jev-latest", "state": state,
                                             "questions": {fid: question(p) for fid, p in ids.items()}})
    scored = sorted(((a["score"], ids[fid]) for fid, a in result["answers"].items()), reverse=True)
    return scored, result["usage"]["cost"]


def numbered(path):
    lines = (ROOT / path).read_text(errors="replace").splitlines()
    return "\n".join(f"{n:4}  {line}" for n, line in enumerate(lines, 1))


DEPTH = {
    "merge": "Triage judged this routine. Keep the comment short: say why it does not affect us, and "
             "mention anything relevant only if the notes or diffs actually touch our configuration.",
    "review": "Triage flagged this for review. Check every note item that could plausibly matter "
              "against the files, and be specific about what is affected.",
    "block": "Triage judged this unsafe to merge as-is. Explain precisely what breaks and where.",
}


def comment(change, paths, action):
    files = "\n\n".join(f"=== {p} ===\n{numbered(p)}" for p in paths)
    prompt = f"""You are writing the pull request comment for a dependency bump in a GitOps (Flux)
homelab Kubernetes repository. The reader is the repository owner deciding whether to merge. They
care only about what this bump means for *their* deployment, not about upstream changes in general.

{DEPTH[action]}

Write GitHub markdown with exactly this structure and nothing before or after it:

### <emoji> <one-line headline>
Use 🟢 when nothing affects this deployment, 🟡 when something affects it but merging is fine with
the noted follow-up, 🔴 when it should not be merged as-is.

**Why:** one to three sentences on the reasoning behind the verdict.

**Affects us:** a bullet per change that touches this deployment, each citing `file:line` from the
files below and saying what it changes for us. If nothing does, write exactly: None.

**Doesn't affect us:** one line summarising the remaining upstream changes and why they are inert
here (e.g. "new opt-in values we don't set; docs and CI changes").

**Before merging:** bullets of concrete actions, only if any are needed. Omit this heading otherwise.

Rules: cite only what the files and diffs below show. When they cannot settle whether something is
handled, say what to check (a command or a file) instead of asserting either way. Do not repeat the
release notes; translate them into consequences for this deployment. A subchart version bump is not
an image change unless the rendered manifest diff shows a different image.

## Pull request
{change['title']}

{change['diff']}

## Notes and diffs
{change['release_notes']}

## Repository files (line-numbered)
{files}
"""
    result = openrouter("/v1/chat/completions", {"model": "typesafe/jev-router",
                                                 "messages": [{"role": "user", "content": prompt}]})
    return (result["choices"][0]["message"]["content"].strip(), result.get("model"),
            result["usage"].get("cost") or 0)


NO_NOTES = """### ⚪ No release notes found — review manually

**Why:** Renovate found no release notes for this package, and it is not a Helm chart bump, so
there is nothing to judge it against. An automated verdict here would be a guess.

If the project publishes notes somewhere Renovate cannot see, a `sourceUrl` packageRule in
`renovate.json` pointing at the upstream repository usually fixes this for future bumps."""


def post(number, body):
    body = f"{MARKER}\n{body}"
    existing = [c for c in github(f"/repos/{REPO}/issues/{number}/comments?per_page=100")
                if MARKER in (c["body"] or "")]
    if existing:
        github(f"/repos/{REPO}/issues/comments/{existing[0]['id']}", {"body": body}, "PATCH")
    else:
        github(f"/repos/{REPO}/issues/{number}/comments", {"body": body}, "POST")


TRIAGE_ONLY = {
    "merge": "### 🟢 Routine bump\n\nTriage found nothing in the notes that needs attention.",
    "review": "### 🟡 Flagged for review\n\nTriage found something in the notes that may affect this "
              "deployment. Check the notes against our configuration before merging.",
    "block": "### 🔴 Flagged as unsafe to merge\n\nTriage judged from the notes that this cannot be merged as-is.",
}


def describe_mode():
    mode = os.environ.get("DESCRIBE_BUMPS") or "flagged"
    if mode not in ("none", "flagged", "all"):
        sys.exit(f"DESCRIBE_BUMPS must be none, flagged or all, not {mode!r}")
    return mode


def main():
    number = os.environ["PR_NUMBER"]
    dry_run = "--dry-run" in sys.argv
    mode = describe_mode()
    change, has_notes = pull_request(number)
    facts = chart_facts(change["diff"])

    if not has_notes and not facts:
        body = NO_NOTES
    else:
        change["release_notes"] += facts
        step1 = triage(change)
        a = step1["answers"]
        action = a["action"]["choice"]
        probabilities = ", ".join(f"{k} {v:.2f}" for k, v in
                                  sorted(a["action"]["probabilities"].items(), key=lambda kv: -kv[1]))
        details = [f"- **Triage** (jev-latest): {action} ({probabilities}), "
                   f"confidence {a['action']['confidence']:.2f}, risk {a['risk']['score']:.2f}"]
        total = step1["usage"]["cost"]

        if mode == "all" or (mode == "flagged" and action != "merge"):
            scored, locate_cost = locate(change)
            picked = [p for s, p in scored[:MAX_FILES] if s >= RELEVANCE_FLOOR]
            body, model, write_cost = comment(change, picked, action)
            total += locate_cost + write_cost
            considered = "\n".join(f"  - `{p}` ({s:.2f})" for s, p in scored if p in picked)
            details += [f"- **Files read** (jev-latest relevance, of {len(scored)}):\n{considered}",
                        f"- **Written by** jev-router → `{model}`"]
        else:
            body = TRIAGE_ONLY[action]
            details.append(f"- **Not described**: `DESCRIBE_BUMPS={mode}`")

        details.append(f"- **Cost** ${total:.4f}")
        body += ("\n\n<details><summary>How this was decided</summary>\n\n" + "\n".join(details) +
                 "\n\nAdvisory only — this comment never approves, merges or blocks.\n</details>")

    if dry_run:
        print(body)
    else:
        post(number, body)


if __name__ == "__main__":
    main()
