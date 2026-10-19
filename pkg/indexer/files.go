package indexer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/qiangli/yoke/pkg/lens"
)

type sourceFile struct{ path, unit, rel, root string }

func repoRoot(path string) string {
	for d := path; ; d = filepath.Dir(d) {
		if _, e := os.Lstat(filepath.Join(d, ".git")); e == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
func canonicalRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, e := url.Parse(raw); e == nil && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.Host = strings.ToLower(u.Host)
		return strings.TrimSuffix(strings.TrimSuffix(u.String(), "/"), ".git")
	}
	if at := strings.Index(raw, "@"); at >= 0 {
		raw = raw[at+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
}

// ignored reads only ancestor ignore files. It never traverses an excluded
// directory just to discover its ignore rules.
func ignored(root, path string, isDir bool) bool {
	rr := repoRoot(root)
	if rr != "" {
		root = rr
	}
	rel, e := filepath.Rel(root, path)
	if e != nil {
		return true
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	var ps []gitignore.Pattern
	read := func(name string, base []string) bool {
		f, e := os.Open(name)
		if os.IsNotExist(e) {
			return true
		}
		if e != nil {
			return false
		}
		b, e := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		f.Close()
		if e != nil || len(b) > 64<<10 {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				ps = append(ps, gitignore.ParsePattern(line, base))
			}
		}
		return true
	}
	// Git's global/local excludes precede per-directory rules. The repository
	// storage filesystem resolves linked worktrees to their common metadata.
	if rr != "" {
		repo, e := gogit.PlainOpenWithOptions(rr, &gogit.PlainOpenOptions{EnableDotGitCommonDir: true})
		if e != nil {
			return true
		}
		cfg, e := repo.ConfigScoped(gitconfig.GlobalScope)
		if e != nil {
			return true
		}
		exclude := cfg.Raw.Section("core").Option("excludesfile")
		if strings.HasPrefix(exclude, "~/") {
			h, _ := os.UserHomeDir()
			exclude = filepath.Join(h, exclude[2:])
		}
		if exclude != "" {
			if !filepath.IsAbs(exclude) {
				exclude = filepath.Join(rr, exclude)
			}
			if !read(exclude, nil) {
				return true
			}
		}
		if st, ok := repo.Storer.(*gitfs.Storage); ok {
			if !read(filepath.Join(st.Filesystem().Root(), "info", "exclude"), nil) {
				return true
			}
		}
	}
	for i := 0; i < len(parts); i++ {
		dir := filepath.Join(append([]string{root}, parts[:i]...)...)
		for _, name := range []string{".gitignore", ".bashyignore"} {
			if !read(filepath.Join(dir, name), parts[:i]) {
				return true
			}
		}
	}
	return gitignore.NewMatcher(ps).Match(parts, isDir)
}

func (m *Manager) files(root string, remotes map[string]string) ([]sourceFile, string, error) {
	unit := root
	if rr := repoRoot(root); rr != "" {
		root = rr
	}
	if _, e := os.Lstat(filepath.Join(root, ".git")); e == nil {
		repo, e := gogit.PlainOpenWithOptions(root, &gogit.PlainOpenOptions{EnableDotGitCommonDir: true})
		if e != nil {
			return nil, "", e
		}
		cfg, e := repo.Config()
		if e != nil {
			return nil, "", e
		}
		if r := cfg.Remotes["origin"]; r != nil && len(r.URLs) > 0 {
			unit = canonicalRemote(r.URLs[0])
		} else {
			unit = root
		}
		if first, ok := remotes[unit]; ok && first != root {
			return nil, "duplicate of " + first, nil
		}
		remotes[unit] = root
		idx, e := repo.Storer.Index()
		if e != nil {
			return nil, "", e
		}
		var out []sourceFile
		for _, ent := range idx.Entries {
			p := filepath.Join(root, filepath.FromSlash(ent.Name))
			st, e := os.Lstat(p)
			if e != nil {
				if os.IsNotExist(e) {
					continue
				}
				return nil, "", e
			}
			if st.IsDir() {
				continue
			}
			if m.excluded(root, p, st) || ignored(root, p, false) {
				continue
			}
			if unsafeParents(root, p) {
				continue
			}
			out = append(out, sourceFile{p, unit, ent.Name, root})
		}
		// Submodules are separate units, not recursively flattened into their parent.
		for name := range cfg.Submodules {
			p := filepath.Join(root, filepath.FromSlash(cfg.Submodules[name].Path))
			st, e := os.Lstat(p)
			if e == nil && st.IsDir() && !m.excluded(root, p, st) && !ignored(root, p, true) && !unsafeParents(root, p) {
				sub, _, e := m.files(p, remotes)
				if e != nil {
					return nil, "", e
				}
				out = append(out, sub...)
			}
		}
		return out, "git", nil
	}
	var out []sourceFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if m.excluded(root, p, st) || ignored(root, p, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, e := os.Lstat(filepath.Join(p, ".git")); e == nil {
				sub, _, e := m.files(p, remotes)
				if e != nil {
					return e
				}
				out = append(out, sub...)
				return filepath.SkipDir
			}
			return nil
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		out = append(out, sourceFile{p, "file", filepath.ToSlash(p), root})
		if len(out) > m.maxFiles() {
			return fmt.Errorf("index: root exceeds %d files", m.maxFiles())
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, "directory", err
}
func unsafeParents(root, path string) bool {
	for p := filepath.Dir(path); p != root; p = filepath.Dir(p) {
		if filepath.Dir(p) == p {
			return true
		}
		st, e := os.Lstat(p)
		if e != nil || st.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
func (m *Manager) readFile(root, p string) ([]byte, error) {
	if unsafeParents(root, p) {
		return nil, fmt.Errorf("index: symlinked parent")
	}
	st, e := os.Lstat(root)
	if e != nil || !st.IsDir() {
		return nil, fmt.Errorf("index: root is unavailable or no longer a directory")
	}
	rr, e := os.OpenRoot(root)
	if e != nil {
		return nil, e
	}
	defer rr.Close()
	opened, e := rr.Stat(".")
	if e != nil || !os.SameFile(st, opened) {
		return nil, fmt.Errorf("index: root changed during read")
	}
	rel, e := filepath.Rel(root, p)
	if e != nil {
		return nil, e
	}
	before, e := rr.Lstat(rel)
	if e != nil {
		return nil, e
	}
	if placeholder(before) || !before.Mode().IsRegular() || before.Size() > m.maxBytes() {
		return nil, fmt.Errorf("index: unsupported size or file type")
	}
	f, e := rr.Open(rel)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(before, actual) {
		return nil, fmt.Errorf("index: source changed during open")
	}
	b, e := io.ReadAll(io.LimitReader(f, m.maxBytes()+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > m.maxBytes() {
		return nil, fmt.Errorf("index: source exceeded size cap")
	}
	return b, nil
}
func (m *Manager) extract(ctx context.Context, root, p string) (string, error) {
	b, e := m.readFile(root, p)
	if e != nil {
		return "", e
	}

	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".zip", ".jar", ".whl", ".tar", ".tgz":
		return m.archive(ctx, p, b)
	case ".gz":
		if strings.HasSuffix(strings.ToLower(p), ".tar.gz") {
			return m.archive(ctx, p, b)
		}
	case ".pdf", ".docx", ".xlsx", ".html", ".htm":
		res, e := lens.Convert(ctx, lens.Request{Name: p, To: "txt", Data: b})
		if e != nil {
			return "", e
		}
		if len(res.Data) > 256<<10 {
			return "", fmt.Errorf("index: extracted text exceeds 256KiB")
		}
		return string(res.Data), nil
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", fmt.Errorf("index: unsupported binary format")
	}
	if len(b) > 256<<10 {
		return "", fmt.Errorf("index: text exceeds 256KiB")
	}
	return string(b), nil
}

// archive never unpacks. Validate entry count and expanded bytes before calling
// lens's shared listing reader, then include bounded eligible text members.
func (m *Manager) archive(ctx context.Context, p string, b []byte) (string, error) {
	var text strings.Builder
	count, total := 0, int64(0)
	add := func(name string, size int64, mode fs.FileMode, r io.Reader) error {
		count++
		total += size
		if count > 512 || size < 0 || total > 8<<20 {
			return fmt.Errorf("index: archive budget exceeded")
		}
		rel := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
			return nil
		}
		// Archive entry exclusions apply before reading any member bytes.
		path := filepath.Join(filepath.Dir(p), rel)
		fake := archiveInfo{name: filepath.Base(path), size: size, mode: mode}
		if m.excluded(filepath.Dir(p), path, fake) || ignored(filepath.Dir(p), path, false) {
			return nil
		}
		if size > 32<<10 {
			return nil
		}
		data, e := io.ReadAll(io.LimitReader(r, (32<<10)+1))
		if e != nil {
			return e
		}
		if len(data) > 32<<10 {
			return fmt.Errorf("index: archive entry grew past cap")
		}
		if utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
			text.WriteString(name)
			text.WriteByte('\n')
			text.Write(data)
			text.WriteByte('\n')
		}
		return nil
	}
	format := "tar"
	if strings.HasSuffix(strings.ToLower(p), ".zip") || strings.HasSuffix(strings.ToLower(p), ".jar") || strings.HasSuffix(strings.ToLower(p), ".whl") {
		format = "zip"
		z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if e != nil {
			return "", e
		}
		for _, f := range z.File {
			if f.UncompressedSize64 > 8<<20 {
				return "", fmt.Errorf("index: archive member too large")
			}
			if count >= 512 {
				return "", fmt.Errorf("index: archive entry cap")
			}
			if !f.Mode().IsRegular() {
				count++
				continue
			}
			r, e := f.Open()
			if e != nil {
				return "", e
			}
			e = add(f.Name, int64(f.UncompressedSize64), f.Mode(), r)
			r.Close()
			if e != nil {
				return "", e
			}
		}
	} else {
		var r io.Reader = bytes.NewReader(b)
		if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			z, e := gzip.NewReader(r)
			if e != nil {
				return "", e
			}
			defer z.Close()
			r = z
		}
		tr := tar.NewReader(io.LimitReader(r, 9<<20))
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return "", e
			}
			if e = add(h.Name, h.Size, h.FileInfo().Mode(), tr); e != nil {
				return "", e
			}
		}
	}
	res, e := lens.Convert(ctx, lens.Request{From: format, To: "listing", Name: p, Data: b})
	if e != nil {
		return "", e
	}
	// Listing is filtered too: excluded entry names do not leak through metadata.
	var listing strings.Builder
	dec := json.NewDecoder(bytes.NewReader(res.Data))
	for dec.More() {
		var ent lens.Entry
		if e := dec.Decode(&ent); e != nil {
			return "", e
		}
		rel := filepath.Clean(filepath.FromSlash(ent.Path))
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		fake := archiveInfo{name: filepath.Base(rel), size: ent.Size}
		if m.excluded(filepath.Dir(p), filepath.Join(filepath.Dir(p), rel), fake) || ignored(filepath.Dir(p), filepath.Join(filepath.Dir(p), rel), ent.Type == "dir") {
			continue
		}
		listing.WriteString(ent.Path)
		listing.WriteByte('\n')
	}
	return listing.String() + text.String(), nil
}

type archiveInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (a archiveInfo) Name() string       { return a.name }
func (a archiveInfo) Size() int64        { return a.size }
func (a archiveInfo) Mode() fs.FileMode  { return a.mode }
func (a archiveInfo) ModTime() time.Time { return time.Time{} }
func (a archiveInfo) IsDir() bool        { return a.mode.IsDir() }
func (a archiveInfo) Sys() any           { return nil }
