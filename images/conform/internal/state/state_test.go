package state

import (
	"testing"
	"time"
)

// An excuse describes the file that was at a path, not the path itself.
// Replacing the file must clear it, or a genuinely new download inherits the
// previous one's verdict and is never processed.
func TestChangedFileForgetsItsExcuse(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := time.Now()
	s.Put("/m/x.mkv", &Record{Size: 100, ModTime: mod, Excused: true, ExcuseReason: "too big"})

	if r := s.Get("/m/x.mkv", 100, mod); r == nil || !r.Excused {
		t.Fatal("excuse did not survive an unchanged file")
	}
	if r := s.Get("/m/x.mkv", 200, mod); r != nil {
		t.Error("excuse survived a size change")
	}

	s.Put("/m/y.mkv", &Record{Size: 100, ModTime: mod, Failures: 2})
	if r := s.Get("/m/y.mkv", 100, mod.Add(time.Second)); r != nil {
		t.Error("failure count survived an mtime change")
	}
}

func TestRecordsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	mod := time.Now().Truncate(time.Second)
	s, _ := Open(dir)
	s.Put("/m/x.mkv", &Record{Size: 1, ModTime: mod, Excused: true})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := again.Get("/m/x.mkv", 1, mod); r == nil || !r.Excused {
		t.Fatal("record did not round-trip through the state file")
	}
}

func TestPruneDropsVanishedPaths(t *testing.T) {
	s, _ := Open(t.TempDir())
	mod := time.Now()
	s.Put("/m/kept.mkv", &Record{Size: 1, ModTime: mod})
	s.Put("/m/deleted.mkv", &Record{Size: 1, ModTime: mod})

	if n := s.Prune(map[string]bool{"/m/kept.mkv": true}); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if s.Get("/m/kept.mkv", 1, mod) == nil {
		t.Error("pruned a path that still exists")
	}
}
