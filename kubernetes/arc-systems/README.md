# ARC — GitHub Actions self-hosted runners

Actions Runner Controller, in two namespaces:

| Namespace | Chart | What lives there |
|-----------|-------|------------------|
| `arc-systems` | `gha-runner-scale-set-controller` | The controller, and the **listener** pod for each scale set |
| `arc-runners` | `gha-runner-scale-set` | The `AutoscalingRunnerSet` and the ephemeral runner pods |

Both are ArgoCD apps (`argocd/apps/arc-systems.yaml`, `argocd/apps/arc-runners.yaml`)
using OCI charts from `ghcr.io/actions/actions-runner-controller-charts/`.

## The runner label is the Helm release name

`runs-on:` must match the **release name**, which for these multi-source apps is
the ArgoCD `Application` name — so today it is `arc-runners`:

```yaml
jobs:
  build:
    runs-on: arc-runners
```

Renaming `argocd/apps/arc-runners.yaml` therefore silently invalidates every
`runs-on:` in `.github/workflows/`. Jobs then queue forever against a label
nothing offers, and nothing anywhere reports an error. The workflows carried
`runs-on: arc-runner-set` for months and never matched anything.

## The 1Password item

Vault `Secrets`, item `Github ARC`, consumed by the `OnePasswordItem` CRDs in
**both** namespaces. The operator maps each field label to a Secret key
verbatim, so the labels must be exactly:

| Field | Value |
|-------|-------|
| `github_app_id` | The App ID. ARC 0.14.1 types this as a string, so a Client ID also works — no reason to change it if the App ID is already there. |
| `github_app_installation_id` | The **installation** ID, not the App ID. Digits only. |
| `github_app_private_key` | The PKCS#1 PEM GitHub issues when you generate the App key, in a multi-line field. |

Find the installation ID at **github.com → Settings → Applications → Configure**
on the app; it is the trailing path segment of the resulting URL
(`https://github.com/settings/installations/12345678`).

### Why a missing field is so hard to diagnose

ARC's credential check is all-or-nothing —
`apis/actions.github.com/v1alpha1/appconfig/appconfig.go`:

```go
func (c *AppConfig) hasGitHubAppAuth() bool {
    return len(c.AppID) > 0 && c.AppInstallationID > 0 && len(c.AppPrivateKey) > 0
}
```

`FromSecret` raises no error when `github_app_installation_id` is absent; it
just leaves the `int64` at `0`. So **one** missing field produces:

```
failed to resolve app config: failed to validate config:
no credentials provided: either a PAT or GitHub App credentials should be provided
```

which reads as though the Secret were empty, and sends you looking at Connect,
at RBAC, at the `itemPath` — anywhere but the one absent field. That is exactly
what kept this scale set `Pending` from April to August 2026.

The failure chain, none of which surfaces as a red light:

```
missing field
  → controller cannot resolve app config
  → no AutoscalingListener is created
  → no listener pod exists in arc-systems
  → nothing polls GitHub, so no runner ever registers
  → jobs sit on "Waiting for a runner to pick up this job"
  → and ArgoCD still reports the app Synced + Healthy
```

ArgoCD's health check only knows the CR applied cleanly. It has no view of
`status.phase`, so a permanently broken scale set looks green.

### The private key must be a PKCS#1 PEM

`github_app_private_key` must be the `.pem` GitHub hands you when you generate
the App key: it starts `-----BEGIN RSA PRIVATE KEY-----` and runs to ~28 lines.
Two ways to get this wrong:

- Taking the key from a 1Password **SSH Key** item. Those are normalised to
  OpenSSH format (`-----BEGIN OPENSSH PRIVATE KEY-----`), which is not ASN.1 at
  all and which ARC cannot parse.
- Storing it in a single-line field, which eats the newlines and leaves the
  base64 body undecodable.

Either way the symptom is no longer "no credentials provided" — the config
validates, ARC gets as far as talking to GitHub, and *then* fails:

```
failed to create JWT for GitHub app: failed to parse RSA private key from PEM:
asn1: structure error: tags don't match (16 vs {class:1 tag:15 ...}) ... pkcs8 @2
```

That is ARC trying PKCS#1, failing, retrying as PKCS#8 and failing again. Check
the format without exposing the key — this prints only the delimiter lines:

```bash
kubectl -n arc-runners get secret arc-github-app \
  -o jsonpath='{.data.github_app_private_key}' | base64 -d | grep -n -- '-----'
```

`BEGIN OPENSSH PRIVATE KEY` means the value came from an SSH Key item and has
to be replaced. `BEGIN RSA PRIVATE KEY` at line 1 or 2 means the framing is
fine — Go's `pem.Decode` skips anything ahead of the BEGIN line, so a stray
leading quote from a bad paste is harmless — and the problem is inside the
base64 body instead.

If the original `.pem` is lost, generate a fresh private key on the App settings
page — an App can hold several, and GitHub downloads the new one immediately.

Paste it straight into the field. A single-line text field flattens the
newlines, `pem.Decode` then finds no block at all, and the error changes to
`invalid key: Key must be a PEM encoded PKCS1 or PKCS8 key` — a *third* distinct
message for what is still a private-key problem. `op` preserves the file exactly:

```bash
op item edit "Github ARC" --vault Secrets \
  "github_app_private_key[text]=$(cat ./your-app.private-key.pem)"
```

## The App's permissions

Valid credentials are not sufficient — the App must also be *allowed* to mint
runner registration tokens. Without it, everything above succeeds and ARC fails
at the last call:

```
POST https://api.github.com/repos/edjeffreys/infrastructure/actions/runners/registration-token
failed (status="403 Forbidden"): Resource not accessible by integration
```

"Resource not accessible by integration" never names the missing permission. For
a **repository**-scoped scale set it is:

| Permission | Level |
|------------|-------|
| Repository → Administration | **Read and write** |
| Repository → Metadata | Read-only (implicit) |

An organisation-scoped scale set needs Organisation → Self-hosted runners
(read/write) instead.

**Granting the permission is two steps.** Editing it on the App's settings page
changes only what the App *requests*; each installation must then approve it.
Go to the installation (Settings → Applications → Configure) and accept the
"Review permission request" banner. Skip that and the 403 continues unchanged
with nothing to suggest the grant did not take.

## Keep the runner image current, or the scale set eats itself

`Outdated` is a **terminal** phase on an `AutoscalingRunnerSet`. From
`controllers/actions.github.com/autoscalingrunnerset_controller.go`:

```go
outdated := autoscalingRunnerSet.Status.Phase == v1alpha1.AutoscalingRunnerSetPhaseOutdated
```

Every rebuild path — creating the scale set, checking the runner group and name,
and `createEphemeralRunnerSet` — is gated behind `!outdated`. The only branch
left calls `cleanUpResources` and returns with no requeue, and nothing ever
writes the phase back to anything else. Once the object is `Outdated` it can
only tear itself down.

What drives it there is the runner binary. GitHub deprecates old runner
versions, and an *ephemeral* runner cannot self-update, so it exits 7:

```go
case cs.State.Terminated.ExitCode == 7: // outdated
    r.markAsOutdated(ctx, ephemeralRunner, log)
```

EphemeralRunner `Outdated` → EphemeralRunnerSet `Outdated` (`len(state.outdated) > 0`)
→ AutoscalingRunnerSet `Outdated` → teardown, including `DELETE` of the scale
set from the Actions service. On the next reconcile it registers a *new* scale
set, starts a runner, gets exit 7 again, and repeats — churning scale set IDs
on GitHub while every workflow sits on "Waiting for a runner".

The symptom is easy to misread as a networking or credentials fault. It is
neither: the controller is talking to GitHub perfectly well, which is exactly
how it manages to deregister itself.

**Recovering** takes both steps, in order:

1. Bump `image:` in `runner-scale-set-values.yaml` and let ArgoCD sync.
2. `kubectl -n arc-runners delete autoscalingrunnerset arc-runners` — the phase
   is terminal, so a fixed image alone changes nothing. ArgoCD recreates it
   (`prune: true, selfHeal: true`) with an empty status.

**Renovate could not see this image.** The `kubernetes` manager only reads
manifest-shaped files, and a Helm values file has no `apiVersion`/`kind`, so the
pin silently went five releases stale. A `regexManager` in `renovate.json` now
covers it. This is the only `*-values.yaml` in the repo carrying an image pin —
everything else takes its images from its chart.

Related: the `argocd` manager builds a chart's depName by appending `chart:` to
the OCI `repoURL`, and both ARC apps have a `repoURL` that already ends in the
chart name. The real depName is therefore doubled
(`.../gha-runner-scale-set/gha-runner-scale-set`), which is why the rule holding
ARC chart updates back has to match unanchored — an `^actions/` anchor missed it
and Renovate failed that lookup on every run.

## `githubConfigSecret` resolves in the runner namespace

`gha-runner-scale-set` looks up `githubConfigSecret` in the
**AutoscalingRunnerSet's own** namespace, not the controller's. With the Secret
only in `arc-systems` you get `failed to get kubernetes secret:
arc-runners/arc-github-app` and the same silent queueing. Hence the same
`OnePasswordItem` deliberately declared in both namespaces.

## Diagnosing "waiting for a runner"

Work down this list; each step depends on the one above.

```bash
# 1. Does the Secret have all three keys? (values not needed)
kubectl -n arc-runners get secret arc-github-app \
  -o go-template='{{range $k,$v := .data}}{{$k}}{{"\n"}}{{end}}'

# 2. Is the controller reconciling, or erroring?
kubectl -n arc-systems logs deploy/arc-systems-gha-rs-controller --tail=30

# 3. THE decisive check — has the scale set registered with GitHub?
#    An empty id means it never has.
kubectl -n arc-runners get autoscalingrunnerset arc-runners \
  -o jsonpath='{.metadata.annotations.runner-scale-set-id}{"\n"}{.status.phase}{"\n"}'

# 4. Is there a listener? It lives in arc-systems, not arc-runners.
kubectl -n arc-systems get pods | grep listener

# 5. Runner pods only appear once a job is queued (minRunners: 0).
kubectl -n arc-runners get pods
```

After changing the 1Password item, force the sync rather than waiting on the
600s poll, then clear the controller's backoff (retries reach ~16m apart):

```bash
kubectl -n onepassword-operator rollout restart deploy/onepassword-connect-operator
kubectl -n arc-systems rollout restart deploy/arc-systems-gha-rs-controller
```

## Jobs fail in seconds with no steps and no logs

A different failure from the one above: the runner pod *does* start, then the
job goes red about a second later. `gh run view --log-failed` prints nothing and
the job has an empty `steps` array, because the worker crashes before the first
step exists to log against. It looks like a registration or credentials fault
and is neither.

The cause is ownership of the Kubernetes-mode work volume. `containerMode.type:
kubernetes` mounts a PVC at `/home/runner/_work`, and a freshly provisioned
block volume (`longhorn`) arrives owned by root while the runner process is uid
1001 — so it cannot create `_work/_tool`:

```
System.UnauthorizedAccessException: Access to the path '/home/runner/_work/_tool' is denied.
```

The fix is `template.spec.securityContext.fsGroup: 1001` in
`runner-scale-set-values.yaml`, which makes the kubelet chown the volume at
mount time. This is only needed for volume types that honour `fsGroup`, and was
invisible while the work volume was on `nfs`, where the TrueNAS export squashed
ownership server-side.

The runner pod is deleted as soon as the job ends, so catch the log while it is
alive — poll for the pod rather than trying to fetch it afterwards:

```bash
while ! kubectl -n arc-runners get pods -o name | head -1 | grep .; do sleep 2; done
kubectl -n arc-runners logs -l actions.github.com/scale-set-name=arc-runners --tail=200
```

## Kubernetes mode forbids container-less jobs by default

`containerMode.type: kubernetes` exists for one workflow — the Kaniko build in
`build-claude-agent.yaml`, which needs its `container:` to run as its own pod.
But the chart's default also makes a `container:` *mandatory* for every job, so
turning it on retroactively broke `validate-manifests.yaml` and
`talos-upgrade.yaml`, which have always run their steps on the runner directly:

```
##[error]Jobs without a job container are forbidden on this runner,
please add a 'container:' to your job
```

`ACTIONS_RUNNER_REQUIRE_JOB_CONTAINER: "false"` in the runner's env restores
the mixed arrangement. The chart only injects its own `"true"` when that name is
absent from the env list, so setting it suppresses the default rather than
producing a duplicate — see `_helpers.tpl` in `gha-runner-scale-set`.

Worth knowing that this failure and the `fsGroup` one above are both delayed
consequences of enabling Kubernetes mode, and neither appears until Flux
reconciles the HelmRelease. A workflow run triggered by the merge itself will
still use the old runner template and can pass, which makes the breakage look
unrelated to the change that caused it.
