// Package assetring is the ring catalog shared by every declarative
// asset bashy carries: skills, tools, models, agents.
//
// An asset is supplied by one or more rings. Later sources shadow
// earlier ones on a name collision, so the merge order is the
// precedence order:
//
//	embedded  → shared → cloud → local
//	(baseline)  (dirs)   (org)   (this host wins)
//
// A host operator's explicit local entry beats an org default, which
// beats the compiled-in baseline. Writes always land in the local ring;
// nothing ever writes back into a shared dir or the cloud overlay.
//
// Two source shapes cover every asset:
//
//   - Folder sources — one directory per entry, identified by a marker
//     file (skills: a directory containing SKILL.md).
//   - File sources — one file per entry, identified by an extension
//     (fleet: codex.yaml). Each file is byte-identical to the asset
//     Content blob an org catalog would serve.
package assetring

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Ring names where an entry came from.
//
// The numeric values are stable identifiers, not a precedence order —
// precedence is the order of Catalog.Sources. RingCloud was appended
// rather than inserted so no persisted Ring value shifted meaning.
type Ring int

const (
	RingEmbedded Ring = iota // compiled into the host binary
	RingShared               // a shared catalog dir (git clone, synced folder) — read-only
	RingLocal                // host-local store (installed, learned, edited)
	RingCloud                // an org catalog overlay, cached locally — read-only
)

func (r Ring) String() string {
	switch r {
	case RingLocal:
		return "local"
	case RingShared:
		return "shared"
	case RingCloud:
		return "cloud"
	default:
		return "embedded"
	}
}

// Source is one ring's supply of entries.
type Source interface {
	Ring() Ring
	Names() ([]string, error)
	Body(name string) ([]byte, bool)      // the entry itself (SKILL.md, codex.yaml)
	File(name, rel string) ([]byte, bool) // a sibling file (reference.md, skill.dhnt)
	Files(name string) ([]string, error)  // every file belonging to the entry (rel paths)
}

// DirSourcer is implemented by sources backed by a real directory, so a
// caller can report where an entry lives on disk.
type DirSourcer interface {
	Dir() string
}

// --- folder sources: one directory per entry ---------------------------

// FolderFS serves entries from an fs.FS whose top-level directories each
// contain marker (e.g. "SKILL.md").
func FolderFS(fsys fs.FS, ring Ring, marker string) Source {
	return folderFS{fsys: fsys, ring: ring, marker: marker}
}

// FolderDir serves the same shape from a real directory. A missing
// directory is an empty source, not an error.
func FolderDir(dir string, ring Ring, marker string) Source {
	return folderDir{dir: dir, ring: ring, marker: marker}
}

type folderFS struct {
	fsys   fs.FS
	ring   Ring
	marker string
}

func (s folderFS) Ring() Ring { return s.ring }

func (s folderFS) Names() ([]string, error) {
	entries, err := fs.ReadDir(s.fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // a missing source is empty, not broken
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := fs.Stat(s.fsys, e.Name()+"/"+s.marker); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s folderFS) Body(name string) ([]byte, bool) { return s.File(name, s.marker) }

func (s folderFS) Files(name string) ([]string, error) {
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("assetring: bad entry name %q", name)
	}
	var out []string
	err := fs.WalkDir(s.fsys, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(name, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (s folderFS) File(name, rel string) ([]byte, bool) {
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(rel, "..") {
		return nil, false
	}
	data, err := fs.ReadFile(s.fsys, name+"/"+rel)
	if err != nil {
		return nil, false
	}
	return data, true
}

type folderDir struct {
	dir    string
	ring   Ring
	marker string
}

func (s folderDir) Ring() Ring  { return s.ring }
func (s folderDir) Dir() string { return s.dir }

func (s folderDir) inner() folderFS {
	return folderFS{fsys: os.DirFS(s.dir), ring: s.ring, marker: s.marker}
}

func (s folderDir) Names() ([]string, error) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, nil
	}
	return s.inner().Names()
}

func (s folderDir) Body(name string) ([]byte, bool) { return s.File(name, s.marker) }

func (s folderDir) File(name, rel string) ([]byte, bool) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, false
	}
	return s.inner().File(name, rel)
}

func (s folderDir) Files(name string) ([]string, error) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, nil
	}
	return s.inner().Files(name)
}

// --- file sources: one file per entry ----------------------------------

// FileFS serves entries from an fs.FS of `<name><ext>` files (ext
// includes the dot, e.g. ".yaml"). The file content IS the entry body.
func FileFS(fsys fs.FS, ring Ring, ext string) Source {
	return fileFS{fsys: fsys, ring: ring, ext: ext}
}

// FileDir serves the same shape from a real directory. A missing
// directory is an empty source, not an error.
func FileDir(dir string, ring Ring, ext string) Source {
	return fileDir{dir: dir, ring: ring, ext: ext}
}

type fileFS struct {
	fsys fs.FS
	ring Ring
	ext  string
}

func (s fileFS) Ring() Ring { return s.ring }

func (s fileFS) Names() ([]string, error) {
	entries, err := fs.ReadDir(s.fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		// A noun with no compiled-in baseline (hosts, people) is an empty
		// ring, not a broken one. Failing here would make every entry of
		// that kind unresolvable.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, s.ext) {
			continue
		}
		names = append(names, strings.TrimSuffix(n, s.ext))
	}
	sort.Strings(names)
	return names, nil
}

func (s fileFS) Body(name string) ([]byte, bool) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, false
	}
	data, err := fs.ReadFile(s.fsys, name+s.ext)
	if err != nil {
		return nil, false
	}
	return data, true
}

// File has no meaning for a single-file entry: there are no siblings.
func (s fileFS) File(string, string) ([]byte, bool) { return nil, false }

func (s fileFS) Files(name string) ([]string, error) {
	if _, ok := s.Body(name); !ok {
		return nil, nil
	}
	return []string{name + s.ext}, nil
}

type fileDir struct {
	dir  string
	ring Ring
	ext  string
}

func (s fileDir) Ring() Ring  { return s.ring }
func (s fileDir) Dir() string { return s.dir }

func (s fileDir) inner() fileFS { return fileFS{fsys: os.DirFS(s.dir), ring: s.ring, ext: s.ext} }

func (s fileDir) Names() ([]string, error) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, nil
	}
	return s.inner().Names()
}

func (s fileDir) Body(name string) ([]byte, bool) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, false
	}
	return s.inner().Body(name)
}

func (s fileDir) File(string, string) ([]byte, bool) { return nil, false }

func (s fileDir) Files(name string) ([]string, error) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, nil
	}
	return s.inner().Files(name)
}

// --- catalog -----------------------------------------------------------

// Row is one merged catalog entry.
type Row[E any] struct {
	Name    string
	Entry   E
	Source  Source
	Shadows bool // this row hides a same-named entry in an earlier ring
}

// Catalog merges sources; on a name collision the LAST source wins.
type Catalog[E any] struct {
	Sources []Source
	// Parse turns an entry body into E. It must degrade rather than fail:
	// a catalog never hides an entry it could not fully understand.
	Parse func(name string, body []byte, src Source) E
}

// Get returns an entry and its winning source, searching from the last
// (highest-precedence) ring backwards.
func (c *Catalog[E]) Get(name string) (E, Source, bool) {
	var zero E
	for i := len(c.Sources) - 1; i >= 0; i-- {
		src := c.Sources[i]
		body, ok := src.Body(name)
		if !ok {
			continue
		}
		return c.Parse(name, body, src), src, true
	}
	return zero, nil, false
}

// Rows returns every merged entry, name-sorted, with shadowing marked.
func (c *Catalog[E]) Rows() ([]Row[E], error) {
	byName := map[string]int{}
	var rows []Row[E]
	for _, src := range c.Sources {
		names, err := src.Names()
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			body, ok := src.Body(n)
			if !ok {
				continue
			}
			row := Row[E]{Name: n, Entry: c.Parse(n, body, src), Source: src}
			if i, dup := byName[n]; dup {
				row.Shadows = true
				rows[i] = row
				continue
			}
			byName[n] = len(rows)
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// ExitCode maps a cobra error to the repo exit convention: 2 for usage,
// 1 for anything else. (Usage errors exit 2 even where GNU uses 1 — a
// documented repo-wide deviation.)
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "accepts ", "requires ", "invalid argument"} {
		if strings.Contains(msg, p) {
			return 2
		}
	}
	return 1
}
