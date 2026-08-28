#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///
"""Find pods that no Cilium policy restricts.

Cilium is deny-by-default *per direction, per endpoint*: an endpoint becomes
deny-by-default for ingress as soon as any policy selecting it has an ingress
section, and likewise for egress. A pod that no policy selects at all is
completely unrestricted.

`default-deny` is the catch-all for exactly that case, so this answers the
question you need before touching it — and before adding a namespace: which
pods are relying on the catch-all rather than on their own policy?

Reads the live cluster, not the repo. A manifest that was never applied does
not restrict anything, and this repo has shipped that mistake twice: neither
kubernetes/cilium/ (until #249) nor kubernetes/argocd/ was referenced by any
ArgoCD Application, so both sat unreconciled. The argocd one was never fixed —
it was deleted with the rest of ArgoCD in wave 14.

    scripts/check-policy-coverage.py

Exits 1 if any pod is unrestricted in either direction.
"""

import collections
import json
import subprocess
import sys

# default-deny does not select these, so nothing here is ever "covered" by it
# and nothing here should be reported as a gap.
EXCLUDED = {"kube-system", "kube-public", "kube-node-lease", "cilium-secrets"}


def kubectl(*args):
    result = subprocess.run(
        ["kubectl", *args, "-o", "json"],
        capture_output=True, text=True, check=False,
    )
    if result.returncode != 0:
        sys.exit(f"kubectl {' '.join(args)} failed: {result.stderr.strip()}")
    return json.loads(result.stdout)["items"]


def selects(selector, labels, namespace):
    """Approximate Cilium's endpointSelector matching.

    Cilium prefixes label keys with a source (`k8s:`, `any:`); the namespace is
    exposed as the reserved key io.kubernetes.pod.namespace rather than as a
    pod label. `reserved:` selectors match special identities such as the
    health endpoint, never a workload pod.
    """
    if not selector:
        return True

    for key, value in (selector.get("matchLabels") or {}).items():
        bare = key.split(":", 1)[-1]
        if key.startswith("reserved:") or bare == "reserved":
            return False
        if bare == "io.kubernetes.pod.namespace":
            if namespace != value:
                return False
        elif labels.get(bare) != value:
            return False

    for expression in selector.get("matchExpressions") or []:
        bare = expression["key"].split(":", 1)[-1]
        operator = expression["operator"]
        values = expression.get("values", [])
        actual = namespace if bare == "io.kubernetes.pod.namespace" else labels.get(bare)
        if operator == "In" and actual not in values:
            return False
        if operator == "NotIn" and actual in values:
            return False
        if operator == "Exists" and actual is None:
            return False
        if operator == "DoesNotExist" and actual is not None:
            return False

    return True


def policies():
    """(scope, namespace, name, has_ingress, has_egress, selector) per rule set.

    Both CiliumNetworkPolicy and CiliumClusterwideNetworkPolicy, because the
    clusterwide ones do most of the work here and are easy to forget: today
    allow-kubelet-probes and allow-proxy-identity each select every pod outside
    the exclusions and carry an ingress rule, so ingress is already
    deny-by-default cluster-wide.

    A policy may carry `specs:` (a list) instead of `spec:`.
    """
    for kind, scope in (("cnp", "namespaced"), ("ccnp", "clusterwide")):
        args = ["get", kind] + (["-A"] if scope == "namespaced" else [])
        for item in kubectl(*args):
            spec = item.get("spec") or {}
            for rules in item.get("specs") or ([spec] if spec else []):
                yield (
                    scope,
                    item["metadata"].get("namespace"),
                    item["metadata"]["name"],
                    bool(rules.get("ingress") or rules.get("ingressDeny")),
                    bool(rules.get("egress") or rules.get("egressDeny")),
                    rules.get("endpointSelector") or {},
                )


def main():
    all_policies = list(policies())

    rows = []
    for pod in kubectl("get", "pods", "-A"):
        meta = pod["metadata"]
        namespace = meta["namespace"]
        if namespace in EXCLUDED:
            continue
        if pod.get("status", {}).get("phase") in ("Succeeded", "Failed"):
            continue
        # hostNetwork pods take the node's identity, not a pod identity, so
        # namespace-scoped selectors do not apply to them.
        if pod.get("spec", {}).get("hostNetwork"):
            continue

        labels = meta.get("labels") or {}
        ingress = egress = False
        for scope, policy_namespace, _, has_ingress, has_egress, selector in all_policies:
            if scope == "namespaced" and policy_namespace != namespace:
                continue
            if selects(selector, labels, namespace):
                ingress = ingress or has_ingress
                egress = egress or has_egress
        rows.append((namespace, meta["name"], ingress, egress))

    summary = collections.defaultdict(lambda: [0, 0, 0])
    for namespace, _, ingress, egress in rows:
        summary[namespace][0] += 1
        summary[namespace][1] += ingress
        summary[namespace][2] += egress

    print(f"{'namespace':<24}{'pods':>6}{'ingress':>9}{'egress':>8}")
    print("-" * 47)
    for namespace in sorted(summary):
        total, ingress, egress = summary[namespace]
        mark = "" if (ingress == total and egress == total) else "   <-- gap"
        print(f"{namespace:<24}{total:>6}{ingress:>9}{egress:>8}{mark}")

    gaps = [row for row in rows if not row[2] or not row[3]]
    print(f"\n{len(rows)} pods checked, {len(gaps)} unrestricted in some direction")
    for namespace, pod, ingress, egress in sorted(gaps):
        missing = ", ".join(
            direction for direction, ok in (("ingress", ingress), ("egress", egress)) if not ok
        )
        print(f"  {namespace}/{pod} -> {missing}")

    sys.exit(1 if gaps else 0)


if __name__ == "__main__":
    main()
