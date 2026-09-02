// Package run executes a plan: encode to a temp file, verify the result, and
// only then replace the original.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
	"github.com/edjeffreys/conform/internal/plan"
	"github.com/edjeffreys/conform/internal/state"
)

type Outcome string

const (
	OutcomeReplaced Outcome = "replaced"
	OutcomeSkipped  Outcome = "skipped"
	OutcomeExcused  Outcome = "excused"
	OutcomeFailed   Outcome = "failed"
)

type Result struct {
	Path     string
	Outcome  Outcome
	Action   plan.Action
	Detail   string
	Before   int64
	After    int64
	Duration time.Duration
}

type Runner struct {
	Exec   config.Execution
	Prober *media.Prober
	Store  *state.Store
	Logf   func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Apply carries out p against its file. It returns a Result for every path it
// touches, including the ones it decides to leave alone; err is reserved for
// faults that should stop the run, not for a file that could not be encoded.
func (r *Runner) Apply(ctx context.Context, p *plan.Plan, prof config.Profile) (Result, error) {
	src := p.File.Path
	res := Result{Path: src, Action: p.Action, Before: p.File.Size}
	start := time.Now()

	if p.Action == plan.ActionNone {
		res.Outcome = OutcomeSkipped
		return res, nil
	}

	tmp, err := r.tempPath(src, p.Container)
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp)

	args := p.FFmpegArgs(src, tmp)
	r.logf("ffmpeg %s", strings.Join(args, " "))
	if out, err := runFFmpeg(ctx, r.Exec.FFmpeg, args); err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return r.fail(src, p, res, fmt.Sprintf("ffmpeg: %v: %s", err, out)), nil
	}

	// Verification is what makes an in-place rewrite safe. Anything short of
	// a fully conformant, complete output leaves the original untouched.
	if detail, ok := r.verify(ctx, p, prof, tmp); !ok {
		return r.reject(src, p, res, detail), nil
	}

	info, err := os.Stat(tmp)
	if err != nil {
		return res, err
	}
	res.After = info.Size()

	final, err := r.replace(tmp, src, p.Container)
	if err != nil {
		return r.fail(src, p, res, err.Error()), nil
	}

	r.Store.Forget(src)
	if final != src {
		r.Store.Forget(final)
	}
	res.Outcome = OutcomeReplaced
	res.Path = final
	res.Duration = time.Since(start)
	return res, nil
}

// verify re-probes the encode and re-plans it against the same profile. The
// second plan must come back as ActionNone: that is the direct proof the file
// now conforms, and it is also what guarantees the next pass will leave it
// alone rather than encoding it again forever.
func (r *Runner) verify(ctx context.Context, p *plan.Plan, prof config.Profile, tmp string) (string, bool) {
	out, err := r.Prober.Probe(ctx, tmp)
	if err != nil {
		return fmt.Sprintf("output is not probeable: %v", err), false
	}

	if p.File.Duration > 0 {
		ratio := out.Duration / p.File.Duration
		if ratio < r.Exec.MinDurationRatio {
			return fmt.Sprintf("output is %.1fs against a %.1fs source (%.3f of it)",
				out.Duration, p.File.Duration, ratio), false
		}
	}

	if again := plan.Build(out, prof); again.Action != plan.ActionNone {
		return fmt.Sprintf("output still does not satisfy the profile (%s) — the profile is likely unsatisfiable, not the file", again), false
	}

	// Size is judged only on a re-encode. A remux can grow slightly from
	// container overhead alone, and rejecting it on that basis would refuse a
	// change that costs nothing and is wanted regardless.
	if p.Action == plan.ActionTranscode && p.File.Size > 0 {
		info, err := os.Stat(tmp)
		if err != nil {
			return err.Error(), false
		}
		if ratio := float64(info.Size()) / float64(p.File.Size); ratio > r.Exec.MaxSizeRatio {
			return fmt.Sprintf("re-encode is %.2fx the size of the source", ratio), false
		}
	}
	return "", true
}

// replace moves the verified encode over the original. The staging copy is
// made in the source's own directory so the final step is a rename within one
// filesystem, which is atomic; the temp dir is usually a different volume, so
// renaming straight from it would not be.
func (r *Runner) replace(tmp, src, container string) (string, error) {
	dir := filepath.Dir(src)
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	final := filepath.Join(dir, base+media.Ext(container))
	staging := filepath.Join(dir, "."+base+".conform"+media.Ext(container))

	if err := copyFile(tmp, staging); err != nil {
		os.Remove(staging)
		return "", fmt.Errorf("stage beside source: %w", err)
	}
	if o := r.Exec.Owner; o != nil {
		if err := os.Chown(staging, o.UID, o.GID); err != nil {
			os.Remove(staging)
			return "", fmt.Errorf("chown staged file: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		os.Remove(staging)
		return "", fmt.Errorf("commit over source: %w", err)
	}
	// A container change moves the file to a new extension, leaving the
	// original behind. Removing it after the rename rather than before means
	// a crash in between leaves two copies, never zero.
	if final != src {
		if err := os.Remove(src); err != nil {
			return final, fmt.Errorf("remove superseded %s: %w", filepath.Base(src), err)
		}
	}
	return final, nil
}

func (r *Runner) fail(src string, p *plan.Plan, res Result, detail string) Result {
	rec := r.Store.Get(src, p.File.Size, p.File.ModTime)
	if rec == nil {
		rec = &state.Record{Size: p.File.Size, ModTime: p.File.ModTime, Probe: p.File}
	}
	rec.Failures++
	rec.LastError = detail
	rec.LastAttempt = time.Now()
	if rec.Failures >= r.Exec.MaxFailures {
		rec.Excused = true
		rec.ExcuseReason = fmt.Sprintf("failed %d times, last: %s", rec.Failures, truncate(detail, 200))
	}
	r.Store.Put(src, rec)

	res.Outcome = OutcomeFailed
	res.Detail = detail
	return res
}

// reject records a verified-but-unacceptable result. Unlike a failure this is
// excused immediately: retrying produces the same output, so the only thing a
// second attempt buys is the GPU time.
func (r *Runner) reject(src string, p *plan.Plan, res Result, detail string) Result {
	r.Store.Put(src, &state.Record{
		Size: p.File.Size, ModTime: p.File.ModTime, Probe: p.File,
		Excused: true, ExcuseReason: detail, LastAttempt: time.Now(),
	})
	res.Outcome = OutcomeExcused
	res.Detail = detail
	return res
}

func (r *Runner) tempPath(src, container string) (string, error) {
	dir := r.Exec.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(src))
	name := fmt.Sprintf("conform-%s%s", hex.EncodeToString(sum[:8]), media.Ext(container))
	return filepath.Join(dir, name), nil
}

// runFFmpeg returns the tail of stderr on failure. ffmpeg writes progress
// there too, so the whole stream is mostly noise; the last lines are where the
// actual error is.
func runFFmpeg(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	err := cmd.Run()
	return tail(stderr.String(), 12), err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// Fsync before the rename: without it the rename can be durable while the
	// contents are not, which on a crash leaves a valid-looking empty file
	// where the original used to be.
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
