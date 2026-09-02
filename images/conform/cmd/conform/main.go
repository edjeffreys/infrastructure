// Command conform brings a media library into line with a declared profile.
//
// It is a reconciler, not a job queue: the desired state is a profile in a
// config file, the observed state is what ffprobe reports about each file, and
// the action is whatever closes the gap. Nothing about a file's history is
// consulted to decide whether it needs work, so the plan is a pure function of
// the library and the config, and running it twice is a no-op.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
	"github.com/edjeffreys/conform/internal/plan"
	"github.com/edjeffreys/conform/internal/run"
	"github.com/edjeffreys/conform/internal/scan"
	"github.com/edjeffreys/conform/internal/state"
)

var version = "dev"

func main() {
	if err := realMain(); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func realMain() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("no command given")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "probe":
		return cmdProbe(ctx, os.Args[2:])
	case "plan":
		return cmdPlan(ctx, os.Args[2:])
	case "apply":
		return cmdApply(ctx, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("conform", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `conform — reconcile a media library against a declared profile

  conform probe <file>      print what ffprobe sees, as conform models it
  conform plan  [flags]     show what would change; touches nothing
  conform apply [flags]     make it so

Flags for plan and apply:
  -config PATH    config file (default conform.yaml)
  -library NAME   restrict to one library
  -limit N        stop after N files needing work
  -verbose        log the ffmpeg command lines

Flags for apply only:
  -dry-run        plan only, but through the apply path
  -retry-excused  reconsider files previously excused after a failure
  -interval D     repeat forever, waiting D between passes (e.g. 6h)
`)
}

// session is the shared setup for plan and apply.
type session struct {
	cfg     *config.Config
	prober  *media.Prober
	store   *state.Store
	limit   int
	library string
	verbose bool
}

func newSession(cfgPath, library string, limit int, verbose bool) (*session, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(cfg.Execution.StateDir)
	if err != nil {
		return nil, err
	}
	return &session{
		cfg: cfg, prober: media.NewProber(cfg.Execution.FFprobe), store: store,
		limit: limit, library: library, verbose: verbose,
	}, nil
}

type item struct {
	lib  config.Library
	prof config.Profile
	plan *plan.Plan
}

// collect probes every file in scope and plans it. Files whose probe is
// cached and unchanged cost a stat; the rest cost an ffprobe.
func (s *session) collect(ctx context.Context, includeExcused bool) ([]item, *tally, error) {
	var items []item
	t := &tally{}
	seen := map[string]bool{}

collect:
	for _, lib := range s.cfg.Libraries {
		if s.library != "" && lib.Name != s.library {
			continue
		}
		entries, err := scan.Walk(lib)
		if err != nil {
			return nil, nil, fmt.Errorf("scan library %q: %w", lib.Name, err)
		}
		prof := s.cfg.Profile(lib)

		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			seen[e.Path] = true
			t.total++

			rec := s.store.Get(e.Path, e.Info.Size(), e.Info.ModTime())
			if rec != nil && rec.Excused && !includeExcused {
				t.excused++
				continue
			}

			f := recProbe(rec)
			if f == nil {
				probed, err := s.prober.Probe(ctx, e.Path)
				if err != nil {
					t.unreadable++
					fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel(lib, e.Path), err)
					continue
				}
				f = probed
				s.store.Put(e.Path, &state.Record{Size: f.Size, ModTime: f.ModTime, Probe: f})
			}

			p := plan.Build(f, prof)
			t.count(p.Action)
			if p.Action == plan.ActionNone {
				continue
			}
			items = append(items, item{lib: lib, prof: prof, plan: p})
			if s.limit > 0 && len(items) >= s.limit {
				break collect
			}
		}
	}

	// Only a full, unfiltered pass knows which paths are genuinely gone.
	if s.library == "" && s.limit == 0 {
		s.store.Prune(seen)
	}
	return items, t, s.store.Save()
}

func recProbe(r *state.Record) *media.File {
	if r == nil {
		return nil
	}
	return r.Probe
}

type tally struct {
	total, none, remux, transcode, excused, unreadable int
}

func (t *tally) count(a plan.Action) {
	switch a {
	case plan.ActionNone:
		t.none++
	case plan.ActionRemux:
		t.remux++
	case plan.ActionTranscode:
		t.transcode++
	}
}

func (t *tally) print() {
	fmt.Printf("\n%d files — %d conformant, %d remux, %d transcode",
		t.total, t.none, t.remux, t.transcode)
	if t.excused > 0 {
		fmt.Printf(", %d excused", t.excused)
	}
	if t.unreadable > 0 {
		fmt.Printf(", %d unreadable", t.unreadable)
	}
	fmt.Println()
}

func cmdProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	ffprobe := fs.String("ffprobe", "ffprobe", "ffprobe binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("probe takes exactly one file")
	}
	f, err := media.NewProber(*ffprobe).Probe(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

func planFlags(fs *flag.FlagSet) (cfg, lib *string, limit *int, verbose *bool) {
	cfg = fs.String("config", "conform.yaml", "config file")
	lib = fs.String("library", "", "restrict to one library")
	limit = fs.Int("limit", 0, "stop after N files needing work")
	verbose = fs.Bool("verbose", false, "log ffmpeg command lines")
	return
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath, library, limit, verbose := planFlags(fs)
	showExcused := fs.Bool("show-excused", false, "include files excused after a failure")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := newSession(*cfgPath, *library, *limit, *verbose)
	if err != nil {
		return err
	}

	items, t, err := s.collect(ctx, *showExcused)
	if err != nil {
		return err
	}
	for _, it := range items {
		fmt.Printf("%-9s %s\n", it.plan.Action, rel(it.lib, it.plan.File.Path))
		for _, why := range it.plan.Reasons {
			fmt.Printf("          · %s\n", why)
		}
		for _, d := range it.plan.Dropped {
			fmt.Printf("          − drop %s stream %d (%s)\n", d.Type, d.Source, d.Reason)
		}
		if *verbose {
			fmt.Printf("          $ %s %v\n", s.cfg.Execution.FFmpeg, it.plan.FFmpegArgs(it.plan.File.Path, "OUTPUT"+media.Ext(it.plan.Container)))
		}
	}
	t.print()
	return nil
}

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	cfgPath, library, limit, verbose := planFlags(fs)
	dryRun := fs.Bool("dry-run", false, "plan only")
	retryExcused := fs.Bool("retry-excused", false, "reconsider previously excused files")
	interval := fs.Duration("interval", 0, "repeat forever, waiting this long between passes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := newSession(*cfgPath, *library, *limit, *verbose)
	if err != nil {
		return err
	}

	for {
		if err := s.pass(ctx, *dryRun, *retryExcused); err != nil {
			return err
		}
		if *interval == 0 {
			return nil
		}
		fmt.Printf("\nnext pass in %s\n", *interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*interval):
		}
	}
}

func (s *session) pass(ctx context.Context, dryRun, retryExcused bool) error {
	items, t, err := s.collect(ctx, retryExcused)
	if err != nil {
		return err
	}
	if dryRun {
		for _, it := range items {
			fmt.Printf("%-9s %s — %s\n", it.plan.Action, rel(it.lib, it.plan.File.Path), it.plan)
		}
		t.print()
		return nil
	}

	// A worker that hits a fatal error stops reading, so the send below would
	// block forever on a context that is still live. Cancelling is what lets
	// the loop unwind instead of deadlocking.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := &run.Runner{Exec: s.cfg.Execution, Prober: s.prober, Store: s.store}
	if s.verbose {
		runner.Logf = func(f string, a ...any) { fmt.Printf("          $ "+f+"\n", a...) }
	}

	work := make(chan item)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures int

	for range s.cfg.Execution.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				res, err := runner.Apply(ctx, it.plan, it.prof)
				mu.Lock()
				if err != nil {
					failures++
					fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel(it.lib, it.plan.File.Path), err)
				} else {
					report(it, res)
					if res.Outcome == run.OutcomeFailed {
						failures++
					}
				}
				mu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}()
	}

	for _, it := range items {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return ctx.Err()
		case work <- it:
		}
	}
	close(work)
	wg.Wait()

	if err := s.store.Save(); err != nil {
		return err
	}
	t.print()
	if failures > 0 {
		return fmt.Errorf("%d file(s) could not be processed", failures)
	}
	return nil
}

func report(it item, res run.Result) {
	name := rel(it.lib, res.Path)
	switch res.Outcome {
	case run.OutcomeReplaced:
		saved := ""
		if res.Before > 0 && res.After > 0 {
			saved = fmt.Sprintf(" %s → %s (%+.0f%%)", human(res.Before), human(res.After),
				(float64(res.After)/float64(res.Before)-1)*100)
		}
		fmt.Printf("%-9s %s%s in %s\n", res.Action, name, saved, res.Duration.Round(time.Second))
	case run.OutcomeExcused:
		fmt.Printf("%-9s %s — %s\n", "excused", name, res.Detail)
	case run.OutcomeFailed:
		fmt.Printf("%-9s %s\n%s\n", "FAILED", name, indent(res.Detail))
	}
}

func rel(lib config.Library, path string) string {
	if r, err := filepath.Rel(lib.Path, path); err == nil {
		return r
	}
	return path
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTP"[exp])
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "          " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
