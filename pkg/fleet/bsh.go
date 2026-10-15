package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/snippet"
)

// bshExt is the extension of a code-outer record: <name>.bsh next to (or
// instead of) <name>.yaml in a local or shared record dir. Its stdout,
// YAML or JSON, is the record body, parsed exactly like the .yaml file.
const bshExt = ".bsh"

// bshTimeout caps one .bsh record evaluation.
const bshTimeout = 10 * time.Second

// withBshRecords wraps a dir-backed file source (the local store and the
// shared dirs — never the embedded baseline) so .bsh records join the
// ring. A same-named .yaml wins; the shadowed .bsh is reported through
// the parse-error path.
func withBshRecords(src assetring.Source, dir string) assetring.Source {
	return &bshSource{Source: src, dir: dir}
}

type bshSource struct {
	assetring.Source
	dir   string
	mu    sync.Mutex
	warns []error
}

func (s *bshSource) warn(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warns = append(s.warns, err)
}

// Warnings drains deferred warnings (a shadowed or refused .bsh record)
// so the catalog parse path can report them.
func (s *bshSource) Warnings() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.warns
	s.warns = nil
	return out
}

// sourceWarnings surfaces a source's deferred warnings; sources without
// any report nothing.
func sourceWarnings(src assetring.Source) []error {
	if w, ok := src.(interface{ Warnings() []error }); ok {
		return w.Warnings()
	}
	return nil
}

func (s *bshSource) bshPath(name string) string {
	return filepath.Join(s.dir, name+bshExt)
}

func (s *bshSource) Names() ([]string, error) {
	names, err := s.Source.Names()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return names, nil
		}
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, bshExt) {
			continue
		}
		base := strings.TrimSuffix(n, bshExt)
		// A .yaml of the same name wins; the shadow is warned about in
		// Body, which the parse path drains.
		if base == "" || seen[base] || validName(base) != nil {
			continue
		}
		seen[base] = true
		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

func (s *bshSource) Body(name string) ([]byte, bool) {
	if body, ok := s.Source.Body(name); ok {
		if _, err := os.Stat(s.bshPath(name)); err == nil {
			s.warn(fmt.Errorf("fleet: %s shadowed by %s in the same directory; the .yaml record wins", s.bshPath(name), name+ext))
		}
		return body, true
	}
	body, ok, err := s.render(name)
	if err != nil {
		// A refused or failed .bsh never hides the entry: the name stays
		// (as a degraded record) and the reason rides the parse-error path.
		s.warn(err)
		return nil, true
	}
	return body, ok
}

func (s *bshSource) Files(name string) ([]string, error) {
	files, err := s.Source.Files(name)
	if err != nil || len(files) > 0 {
		return files, err
	}
	if _, err := os.Stat(s.bshPath(name)); err == nil {
		return []string{name + bshExt}, nil
	}
	return nil, nil
}

// render evaluates a .bsh record and returns its stdout. ok is false when
// no .bsh exists for the name.
func (s *bshSource) render(name string) ([]byte, bool, error) {
	if validName(name) != nil {
		return nil, false, nil
	}
	path := s.bshPath(name)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	effects, err := bshEffects(path, content)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if cache := bshCachePath(path, content, info.ModTime()); cache != "" {
		if cached, err := os.ReadFile(cache); err == nil {
			// A cache hit never executes the record: model list stays
			// fast and side-effect free after the first evaluation.
			return cached, true, nil
		}
	}
	r, err := snippet.Run(context.Background(), snippet.Snippet{
		Lang:    "bsh",
		Body:    string(content),
		Effects: effects,
		Timeout: bshTimeout,
	}, snippet.IO{Dir: s.dir})
	if err != nil {
		return nil, false, fmt.Errorf("fleet: %s: %w", path, err)
	}
	if len(r.Refused) > 0 {
		return nil, false, fmt.Errorf("fleet: %s: effects refused: %s", path, strings.Join(r.Refused, ", "))
	}
	if r.Exit != 0 {
		return nil, false, fmt.Errorf("fleet: %s: exit %d: %s", path, r.Exit, strings.TrimSpace(string(r.Stderr)))
	}
	if cache := bshCachePath(path, content, info.ModTime()); cache != "" {
		writeBshCache(cache, r.Stdout)
	}
	return r.Stdout, true, nil
}

// bshEffects reads a .bsh record's declared effects from its first line,
// a comment '# effects: pure' or '# effects: read'. A record may not do
// more than read; anything else is refused before anything runs. Without
// a declaration the record runs as read.
func bshEffects(path string, content []byte) ([]string, error) {
	first := content
	if i := bytes.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	decl, ok := strings.CutPrefix(strings.TrimSpace(string(first)), "# effects:")
	if !ok {
		return []string{"read"}, nil
	}
	var effects []string
	for _, f := range strings.FieldsFunc(strings.ToLower(decl), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if f != "pure" && f != "read" {
			return nil, fmt.Errorf("fleet: %s: effect %q refused: a .bsh record may declare only '# effects: pure' or '# effects: read'", path, f)
		}
		if !contains(effects, f) {
			effects = append(effects, f)
		}
	}
	if len(effects) == 0 {
		return []string{"read"}, nil
	}
	return effects, nil
}

// bshCacheDir is where evaluated .bsh records are cached. A variable so
// tests can pin it to a scratch dir.
var bshCacheDir = func() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "bashy", "fleet-bsh"), nil
}

// bshCachePath keys a record's evaluated output by its path, content and
// mtime. "" means caching is unavailable, not broken.
func bshCachePath(path string, content []byte, mtime time.Time) string {
	dir, err := bshCacheDir()
	if err != nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", path, mtime.UnixNano())
	h.Write(content)
	return filepath.Join(dir, hex.EncodeToString(h.Sum(nil))+ext)
}

// writeBshCache stores an evaluated record best-effort: a failed cache
// write never fails the read.
func writeBshCache(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bsh-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = renameOver(tmpName, path)
}
