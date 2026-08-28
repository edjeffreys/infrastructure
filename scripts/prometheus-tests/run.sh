#!/usr/bin/env bash
# Unit-test the PrometheusRules in kubernetes/ with promtool.
#
# Deliberately outside kubernetes/: ArgoCD prunes that tree, and a promtool
# test file is not a Kubernetes manifest — kubeconform would reject it and
# ArgoCD would try to apply it. Same reasoning as scripts/cnpg-migration/.
#
# Not wired into CI. promtool is a ~100 MB download and these rules change
# rarely; run it by hand when you touch them.
#
#   scripts/prometheus-tests/run.sh
#
# Requires `promtool` (from a Prometheus release) and `uv` on PATH.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# promtool reads plain rule files, not PrometheusRule custom resources, so
# extract each .spec into its own file named after the manifest. The test
# files' `rule_files:` entries refer to those names.
uv run --script - "$root" "$work" <<'PY'
# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
import pathlib, sys, yaml

root, work = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
for path in sorted((root / "kubernetes").rglob("*.yaml")):
    try:
        docs = list(yaml.safe_load_all(path.read_text()))
    except yaml.YAMLError:
        continue
    for doc in docs:
        if isinstance(doc, dict) and doc.get("kind") == "PrometheusRule":
            out = work / f"{path.stem}.rules.yaml"
            out.write_text(yaml.safe_dump(doc["spec"], sort_keys=False))
            print(f"extracted {path.relative_to(root)} -> {out.name}")
PY

cp "$here"/*.test.yaml "$work"/
cd "$work"

promtool check rules ./*.rules.yaml
for test in *.test.yaml; do
  echo "== $test"
  promtool test rules "$test"
done
