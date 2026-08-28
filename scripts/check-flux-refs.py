#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Check that every Flux object points at something that exists.

kubeconform validates the *shape* of a Kustomization or HelmRelease, not what
it references. A Kustomization whose `path:` names a directory somebody renamed
is perfectly valid YAML — it just goes NotReady in the cluster after merge,
with no UI to notice it in. Same for a `sourceRef` naming a HelmRepository that
was never written, a `dependsOn` pointing at a deleted app, or a `valuesFrom`
ConfigMap no configMapGenerator produces.

Also enforces the one convention that fails *silently* rather than loudly:
Renovate's `flux` manager will not infer a namespace from the parent
Kustomization, so a HelmRelease or chart source without an explicit
`metadata.namespace` stops receiving chart updates and says nothing about it.

This was one of a pair; check-argocd-refs.py did the same for ArgoCD
Applications and was deleted with them in wave 14.

Run from the repo root. Exits 1 and prints every problem.
"""

import pathlib
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
FLUX = ROOT / "flux"
KUBERNETES = ROOT / "kubernetes"

KUSTOMIZE_GROUP = "kustomize.toolkit.fluxcd.io"
HELM_GROUP = "helm.toolkit.fluxcd.io"
SOURCE_GROUP = "source.toolkit.fluxcd.io"

# Kinds that a sourceRef/chartRef may legitimately name.
SOURCE_KINDS = {"GitRepository", "HelmRepository", "OCIRepository", "HelmChart", "Bucket"}
# Kinds a dependsOn may name. Flux allows a Kustomization to depend on a
# HelmRelease and vice versa, so both are in scope.
DEPENDS_KINDS = {"Kustomization", "HelmRelease"}


def group_of(doc):
    return doc.get("apiVersion", "").split("/")[0]


def load(path, problems):
    try:
        return [d for d in yaml.safe_load_all(path.read_text()) if isinstance(d, dict)]
    except yaml.YAMLError as err:
        problems.append(f"{path.relative_to(ROOT)}: unparseable: {err}")
        return []


def generated_configmaps():
    """Every ConfigMap name a kustomization.yaml under kubernetes/ generates.

    Helm values reach a HelmRelease through a configMapGenerator rather than
    ArgoCD's `$values/` git ref, which has no Flux equivalent.
    """
    names = set()
    for path in KUBERNETES.rglob("kustomization.yaml"):
        try:
            doc = yaml.safe_load(path.read_text()) or {}
        except yaml.YAMLError:
            continue
        for generator in doc.get("configMapGenerator", []) or []:
            if isinstance(generator, dict) and generator.get("name"):
                names.add(generator["name"])
    return names


def main():
    problems = []
    files = sorted(FLUX.rglob("*.yaml"))
    if not files:
        sys.exit(f"no Flux manifests found under {FLUX}")

    docs = []          # (relative path, doc)
    defined = set()    # (kind, namespace, name)
    for file in files:
        relative = file.relative_to(ROOT)
        for doc in load(file, problems):
            docs.append((relative, doc))
            kind = doc.get("kind")
            meta = doc.get("metadata", {})
            defined.add((kind, meta.get("namespace"), meta.get("name")))

    configmaps = generated_configmaps()

    for relative, doc in docs:
        kind = doc.get("kind")
        group = group_of(doc)
        meta = doc.get("metadata", {})
        name = meta.get("name", "<unnamed>")
        namespace = meta.get("namespace")
        spec = doc.get("spec", {})
        where = f"{relative}: {kind}/{name}"

        if group not in (KUSTOMIZE_GROUP, HELM_GROUP, SOURCE_GROUP):
            continue

        # Renovate reads none of these without an explicit namespace, and Flux
        # itself defaults to the controller's namespace rather than erroring.
        if not namespace:
            problems.append(f"{where}: no metadata.namespace")

        if kind == "Kustomization" and group == KUSTOMIZE_GROUP:
            path = spec.get("path")
            if not path:
                problems.append(f"{where}: no spec.path")
            else:
                directory = ROOT / path.lstrip("./")
                if not directory.is_dir():
                    problems.append(f"{where}: path {path!r} is not a directory")

        # sourceRef on a Kustomization, chart.spec.sourceRef and chartRef on a
        # HelmRelease all have to name something this repo actually declares.
        refs = []
        if isinstance(spec.get("sourceRef"), dict):
            refs.append(spec["sourceRef"])
        if isinstance(spec.get("chartRef"), dict):
            refs.append(spec["chartRef"])
        chart_source = spec.get("chart", {}).get("spec", {}).get("sourceRef")
        if isinstance(chart_source, dict):
            refs.append(chart_source)

        for ref in refs:
            ref_kind = ref.get("kind")
            ref_name = ref.get("name")
            ref_namespace = ref.get("namespace", namespace)
            if ref_kind not in SOURCE_KINDS:
                problems.append(f"{where}: sourceRef kind {ref_kind!r} is not a Flux source")
                continue
            # A HelmChart is created by the controller, not written by hand.
            if ref_kind == "HelmChart":
                continue
            if (ref_kind, ref_namespace, ref_name) not in defined:
                problems.append(
                    f"{where}: references {ref_kind}/{ref_name} in "
                    f"{ref_namespace!r}, which is not declared under flux/"
                )

        for dependency in spec.get("dependsOn", []) or []:
            if not isinstance(dependency, dict):
                continue
            dependency_namespace = dependency.get("namespace", namespace)
            dependency_name = dependency.get("name")
            # dependsOn carries no kind — it means "same kind as me" unless the
            # object is a HelmRelease depending on a Kustomization, so accept
            # either rather than reporting a false positive.
            if not any(
                (k, dependency_namespace, dependency_name) in defined
                for k in DEPENDS_KINDS
            ):
                problems.append(
                    f"{where}: dependsOn {dependency_name!r} in "
                    f"{dependency_namespace!r}, which is not declared under flux/"
                )

        for source in spec.get("valuesFrom", []) or []:
            if not isinstance(source, dict) or source.get("kind") != "ConfigMap":
                continue
            values_name = source.get("name")
            if values_name not in configmaps:
                problems.append(
                    f"{where}: valuesFrom ConfigMap {values_name!r} is produced by "
                    f"no configMapGenerator under kubernetes/"
                )

    for problem in problems:
        print(problem, file=sys.stderr)
    print(f"checked {len(files)} files under flux/ — {len(problems)} problem(s)")
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
