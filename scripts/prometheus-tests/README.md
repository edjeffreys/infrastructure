# PrometheusRule unit tests

`promtool test rules` cases for the alerts in `kubernetes/*/`. Run them after
touching any `PrometheusRule`:

```bash
scripts/prometheus-tests/run.sh
```

Needs `promtool` (from a [Prometheus release](https://github.com/prometheus/prometheus/releases))
and `uv` on `PATH`. Not part of CI — promtool is a ~100 MB download and these
rules change rarely.

## Why the Flux tests exist

Flux has no web UI, so `kubernetes/monitoring/flux-monitoring.yaml` is the only
thing that will tell you an app has stopped reconciling. Two of its alerts have
properties that are easy to get wrong and impossible to eyeball:

- **`FluxControllersAbsent` must not fire when Flux is not installed.**
  `absent()` on its own fires on every cluster that has never run Flux. It is
  gated on kube-state-metrics still reporting Flux custom resources, which come
  from the API server rather than from the controllers and so survive the
  controllers being deleted. The test pins both halves.
- **`FluxResourceNotReady` must survive a churning `revision` label.**
  `gotk_resource_info` carries the applied revision, so every push creates a new
  series. Without aggregating that label away the `for: 15m` timer restarts on
  each commit and a genuinely broken app never alerts. The test drives a
  revision change through the middle of the window.
