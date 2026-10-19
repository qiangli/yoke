package indexer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/qiangli/yoke/pkg/kbmemory"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func sourceURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
func sourcePath(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "file" || u.Host != "" {
		return "", fmt.Errorf("index: local file URI required")
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p), nil
}

// Source returns only indexed, scrubbed text, after rechecking today's root
// and exclusion policy. It never opens arbitrary original file contents.
func (m *Manager) Source(ctx context.Context, uri string) (kbmemory.Record, error) {
	m = &Manager{dir: m.dir, Now: m.Now, Embed: m.Embed}
	if e := m.load(); e != nil {
		return kbmemory.Record{}, e
	}
	if len(uri) == 64 {
		if _, e := hex.DecodeString(uri); e == nil {
			docs, e := m.snapshot(ctx)
			if e != nil {
				return kbmemory.Record{}, e
			}
			if d, ok := docs[uri]; ok {
				var r kbmemory.Record
				if e = json.Unmarshal(d.Doc, &r); e != nil {
					return r, e
				}
				return m.Source(ctx, r.Source.Ref)
			}
			return kbmemory.Record{}, fmt.Errorf("index: source not indexed")
		}
	}
	p, e := sourcePath(uri)
	if e != nil {
		return kbmemory.Record{}, e
	}
	st, e := os.Lstat(p)
	if e != nil {
		return kbmemory.Record{}, e
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return kbmemory.Record{}, fmt.Errorf("index: symlink source is not eligible")
	}
	resolved, e := filepath.EvalSymlinks(p)
	if e != nil {
		return kbmemory.Record{}, e
	}
	p = resolved
	uri = sourceURI(p)

	allowed := false
	for _, r := range m.cfg.Roots {
		if (r.State == "watching" || r.State == "unwatched") && inside(r.Path, p) && !m.excluded(r.Path, p, st) && !ignored(r.Path, p, false) && !unsafeParents(r.Path, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		return kbmemory.Record{}, fmt.Errorf("index: source excluded or outside registered roots")
	}
	docs, e := m.snapshot(ctx)
	if e != nil {
		return kbmemory.Record{}, e
	}
	for _, d := range docs {
		var rec kbmemory.Record
		if e = json.Unmarshal(d.Doc, &rec); e != nil {
			return rec, e
		}
		if rec.Source.Ref == uri {
			return rec, nil
		}
	}
	return kbmemory.Record{}, fmt.Errorf("index: source is not indexed")
}
