// Package scan walks a library and yields the files a profile applies to.
package scan

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edjeffreys/conform/internal/config"
)

type Entry struct {
	Path string
	Info fs.FileInfo
}

// Walk returns every file under lib.Path matching its extension list and not
// matching any exclude pattern, in lexical order so runs are reproducible.
func Walk(lib config.Library) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(lib.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable directory should not abort the library —
			// on NFS this is usually a transient mount or permission blip.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// Hidden directories, and the sidecar directories NAS software
			// scatters through media trees, hold no media worth planning.
			if path != lib.Path && (strings.HasPrefix(name, ".") || name == "@eaDir" || name == "lost+found") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if !slices.Contains(lib.Extensions, strings.ToLower(filepath.Ext(name))) {
			return nil
		}

		rel, relErr := filepath.Rel(lib.Path, path)
		if relErr != nil {
			rel = path
		}
		for _, pat := range lib.Exclude {
			if ok, _ := filepath.Match(pat, rel); ok {
				return nil
			}
			if ok, _ := filepath.Match(pat, name); ok {
				return nil
			}
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, Entry{Path: path, Info: info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}
