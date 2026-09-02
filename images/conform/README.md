# conform

A declarative replacement for Tdarr: bring a media library into line with a
profile written in git, with no GUI and no database of jobs.

Not deployed yet — this is the program and its container, built to be tried
locally first. There is no `kubernetes/conform/` or `flux/apps/conform.yaml`,
and nothing here touches the cluster.

## Why not Tdarr

Tdarr's flows, plugins and library settings live in its own MongoDB, edited
through a web UI. The container is declarable; everything that decides what it
actually *does* is not. That is the whole reason this exists.

## The model

conform is a reconciler, not a queue.

| | |
|---|---|
| **Desired state** | a profile in `conform.yaml` |
| **Observed state** | what `ffprobe` reports about the file |
| **Action** | whatever closes the gap |

Nothing about a file's history is consulted to decide whether it needs work, so
`conform plan` is a pure function of the library and the config, and running
`apply` twice is a no-op. That property is not incidental — it is what lets the
config be the only source of truth, and it is enforced rather than assumed: the
runner re-probes and re-plans every output before committing it, and refuses
anything that does not come back clean.

There is no job database because the library *is* the database. A file is
non-conformant if and only if its own streams say so.

### The two things that do need state

A pure reconcile has one failure mode: a file that can never satisfy the
profile is retried forever. Two cases hit it —

- ffmpeg cannot process the file at all;
- the re-encode comes out *larger* than the source, so keeping the original is
  the better outcome and the change is rejected.

Both are recorded in `conform-state.json` as excuses, alongside a probe cache.
Every record is keyed on the file's size and mtime, so replacing the file at a
path discards its excuse with it — a new download is judged on its own merits,
never on its predecessor's.

## Trying it locally

Needs Go and ffmpeg. `conform.local.yaml` uses `libx265` and a `./media`
directory, so it runs anywhere:

```sh
go build -o conform ./cmd/conform

./conform probe media/some-file.mkv      # what conform sees
./conform plan  -config conform.local.yaml   # what it would do; touches nothing
./conform apply -config conform.local.yaml   # do it
./conform plan  -config conform.local.yaml   # must now be a no-op
```

`plan` is read-only and always safe. `apply -dry-run` goes through the apply
path without writing. Add `-limit 1` to try exactly one file, and `-verbose` to
see the ffmpeg command lines.

Run the tests with `go test ./...`.

## Config

`conform.example.yaml` is the homelab shape (QuickSync, NFS paths).
`conform.local.yaml` is the same rules with a software encoder.

Every rule is a predicate on what is **acceptable**, never an instruction to
act. A file satisfying all of them is left alone. Rules left empty impose no
constraint.

```yaml
profiles:
  standard:
    container: mkv         # anything else is remuxed
    video:
      codecs: [hevc]       # acceptable as-is; anything else is re-encoded
      maxHeight: 1080      # taller is downscaled
      encoder: {name: hevc_qsv, options: {global_quality: "24"}}
    audio:
      languages: [eng, und]   # other languages are dropped
      codecs: [aac, ac3, eac3]
      maxChannels: 6          # wider is downmixed
      encoder: {name: eac3, options: {b: "640k"}}
    subtitles:
      languages: [eng]
      codecs: [subrip, ass, hdmv_pgs_subtitle]
```

Options are emitted with a full stream specifier (`-crf:v:0`, not `-crf`), so a
profile stays correct on a file with more than one video track. Keys are sorted,
so a given profile always produces byte-identical arguments.

`execution.tempDir` must not be a Longhorn volume: a transcode writes a full
working copy of every file it processes, and Longhorn would put two replicas of
that on the single Proxmox physical disk. Same reasoning as `tdarr/cache-pvc.yaml`.

### Choices worth knowing about

- **Subtitles are dropped, never converted.** Turning image-based PGS into text
  needs OCR; guessing at it corrupts subtitles silently rather than failing.
- **A language filter that matches nothing is ignored.** Otherwise a mis-tagged
  file would be stripped of its only audio track.
- **Cover art is carried, not judged.** ffprobe reports it as a video stream, so
  measuring it against the video rules would mark every file with an embedded
  poster as needing a re-encode of one still frame.
- **A file with no video stream is never touched**, whatever its container.
- **Size is only checked on a re-encode.** A remux can grow slightly from
  container overhead alone, and refusing it on that basis would block a change
  that costs nothing.

## Replacing a file

The original is never opened for writing. conform encodes to `tempDir`, verifies,
copies the result into the source's *own* directory as a hidden staging file, and
renames over the original — a rename within one filesystem, which is atomic. The
temp directory is usually a different volume, so renaming straight from it would
not be.

A container change writes the new extension and removes the old file afterwards,
so a crash in between leaves two copies rather than none.

Verification is three checks, all of which must pass:

1. the output probes cleanly;
2. its duration is at least `minDurationRatio` of the source — ffmpeg exits 0
   after writing a truncated file more often than it reports an error;
3. re-planning the output returns `none`.

Check 3 is the important one. It is direct proof that the file now conforms,
and therefore that the next pass will leave it alone. If it fails, the profile
is unsatisfiable rather than the file being bad, and the message says so.

## Deploying it later

The pieces that exist: the program, its tests, and a `Dockerfile` targeting
QuickSync. The QSV path is **untested** — it needs the real iGPU, so the base
image and driver setup are a best effort until it runs on `talos-worker-0`.

Still to do when it moves to the cluster:

- a build workflow, copying `.github/workflows/build-claude-agent.yaml` — it
  builds against the in-cluster buildkitd and needs no checkout;
- `kubernetes/conform/` with a namespace, the media PV/PVC, an `nfs` cache PVC,
  a `configMapGenerator` for `conform.yaml`, and `network-policy.yaml`
  (`allow-dns` only — conform talks to nothing);
- `flux/apps/conform.yaml`;
- **an i915 device**, which the cluster does not currently have spare. The
  plugin advertises `gpu.intel.com/i915: 2` and plex and tdarr-node hold one
  each, so either `sharedDevNum` goes to 3 in
  `kubernetes/intel-gpu/daemonset.yaml` or tdarr-node scales to 0. Without one
  the pod sits `Pending` with no other symptom.

The module path is already `github.com/edjeffreys/conform`, so moving this
directory to its own repo is a `git mv` with no import rewrites.
