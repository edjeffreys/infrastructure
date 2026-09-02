// Package state persists the two things that cannot be derived from the
// library itself.
//
// The reconcile is otherwise stateless — a file's own streams are the record
// of whether it conforms — but two cases would never converge without a
// ledger. A file ffmpeg cannot process would be retried on every pass forever,
// and so would one whose re-encode comes out larger than the source and is
// therefore rejected. Both are recorded here as excuses, and both are
// forgotten the moment the file's size or mtime changes, because that means a
// genuinely different file now sits at that path.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edjeffreys/conform/internal/media"
)

type Record struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	// Probe caches the observed state, so a rescan of a large library costs
	// one stat per unchanged file instead of one ffprobe.
	Probe *media.File `json:"probe,omitempty"`

	Failures    int       `json:"failures,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastAttempt time.Time `json:"lastAttempt,omitempty"`

	Excused      bool   `json:"excused,omitempty"`
	ExcuseReason string `json:"excuseReason,omitempty"`
}

type Store struct {
	path    string
	mu      sync.Mutex
	records map[string]*Record
	dirty   bool
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "conform-state.json"), records: map[string]*Record{}}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.records); err != nil {
		// A corrupt state file costs a full re-probe, which is slow but
		// correct. Refusing to start would be worse: nothing here is
		// authoritative, so there is nothing to recover.
		s.records = map[string]*Record{}
	}
	return s, nil
}

// Get returns the record for path only if it still describes the file now at
// that path. A changed size or mtime discards the cached probe, the failure
// count and any excuse together — they all described the previous file.
func (s *Store) Get(path string, size int64, mod time.Time) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[path]
	if !ok {
		return nil
	}
	if r.Size != size || !r.ModTime.Equal(mod) {
		delete(s.records, path)
		s.dirty = true
		return nil
	}
	return r
}

func (s *Store) Put(path string, r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[path] = r
	s.dirty = true
}

// Forget drops a path outright, used after a file is replaced: the new file
// must be probed fresh rather than inherit the old one's history.
func (s *Store) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, path)
	s.dirty = true
}

// Prune removes records for paths no longer present in the library, so a
// long-lived state file does not grow without bound as media is deleted.
func (s *Store) Prune(seen map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p := range s.records {
		if !seen[p] {
			delete(s.records, p)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

// Save writes the store out via a temp file and a rename, so a crash mid-write
// leaves the previous state intact rather than a truncated file.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	s.dirty = false
	return nil
}
