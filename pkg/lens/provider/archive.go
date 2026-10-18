package provider

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

// Limits bound what one locator may make a provider read. Nothing is ever
// extracted to disk; these cap memory and decompression instead.
type Limits struct {
	MaxEntries int   // members indexed in one container
	MaxBuffer  int64 // bytes held in memory for one nested container
	MaxMember  int64 // bytes one Open may yield (decompression bound)
	MaxDepth   int   // nested containers in one locator
}

// DefaultLimits are the limits NewArchive and NewGit start from.
var DefaultLimits = Limits{MaxEntries: 1 << 17, MaxBuffer: 64 << 20, MaxMember: 1 << 30, MaxDepth: 8}

// Archive serves tar:, zip: and gzip: locators. The scheme names the outer
// format; each `!/` before the last one descends into a member whose format
// is declared by its name suffix (see ContainerFormat). The last inner
// component is the path inside the innermost container; `!/.` is its root.
type Archive struct {
	Base   string // directory relative targets resolve against
	Limits Limits
	chains chainCache
}

func NewArchive(base string) *Archive {
	if base == "" {
		base = "."
	}
	return &Archive{Base: base, Limits: DefaultLimits}
}

// ContainerFormat is the deterministic suffix rule for descending into a
// member: bytes are never sniffed to pick a format. "" means not a container.
func ContainerFormat(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"), strings.HasSuffix(n, ".jar"):
		return "zip"
	case strings.HasSuffix(n, ".tar"), strings.HasSuffix(n, ".tgz"), strings.HasSuffix(n, ".tar.gz"):
		return "tar"
	case strings.HasSuffix(n, ".gz"):
		return "gzip"
	}
	return ""
}

func (a *Archive) source(loc ref.Locator) (blob, error) {
	switch loc.Scheme {
	case "tar", "zip", "gzip":
	default:
		return blob{}, ErrUnsupported
	}
	if loc.ScopeKind != "" || loc.Target == "" {
		return blob{}, ErrUnsupported
	}
	p := loc.Target
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.Base, p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return blob{}, err
	}
	if !info.Mode().IsRegular() {
		return blob{}, fmt.Errorf("provider: %s: not a regular file", loc.Target)
	}
	return blob{
		name: filepath.Base(p), size: info.Size(),
		open: func() (io.ReadCloser, error) { return os.Open(p) },
		at: func() (io.ReaderAt, io.Closer, error) {
			f, err := os.Open(p)
			return f, f, err
		},
	}, nil
}

func (a *Archive) resolve(ctx context.Context, loc ref.Locator) (*archiveFS, string, error) {
	src, err := a.source(loc)
	if err != nil {
		return nil, "", err
	}
	return a.chains.descend(ctx, a.Limits, loc.Scheme+":"+loc.Target, src, loc.Scheme, loc.Inner)
}

func (a *Archive) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	fsys, p, err := a.resolve(ctx, loc)
	if err != nil {
		return Node{}, err
	}
	return fsys.node(loc, p)
}

func (a *Archive) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	fsys, p, err := a.resolve(ctx, loc)
	if err != nil {
		return nil, err
	}
	return fsys.list(ctx, loc, p)
}

func (a *Archive) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	fsys, p, err := a.resolve(ctx, loc)
	if err != nil {
		return nil, err
	}
	return fsys.openMember(p)
}

func (a *Archive) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := a.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	return containsEdges(loc, children), nil
}

func containsEdges(loc ref.Locator, children []Node) []Edge {
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "contains", Dst: child.Ref, Attrs: map[string]any{}})
	}
	return edges
}

// blob is a byte source a container is indexed from: re-openable as a
// stream (tar, gzip) and optionally as a ReaderAt (zip).
type blob struct {
	name string
	size int64
	open func() (io.ReadCloser, error)
	at   func() (io.ReaderAt, io.Closer, error)
}

func bytesBlob(name string, data []byte) blob {
	return blob{
		name: name, size: int64(len(data)),
		open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		at:   func() (io.ReaderAt, io.Closer, error) { return bytes.NewReader(data), io.NopCloser(nil), nil },
	}
}

// chainCache memoizes indexes per container spelling so a walk that Stats
// and Lists every directory indexes each container once.
type chainCache struct {
	mu sync.Mutex
	m  map[string]*archiveFS
}

func (c *chainCache) get(key string, build func() (*archiveFS, error)) (*archiveFS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.m[key]; ok {
		return f, nil
	}
	f, err := build()
	if err != nil {
		return nil, err
	}
	if c.m == nil {
		c.m = map[string]*archiveFS{}
	}
	c.m[key] = f
	return f, nil
}

// descend indexes src as format, then follows every inner component but the
// last as a nested container; the last is the path inside the innermost one.
func (c *chainCache) descend(ctx context.Context, lim Limits, key string, src blob, format string, inner []string) (*archiveFS, string, error) {
	if len(inner) > 1 && len(inner)-1 > lim.MaxDepth {
		return nil, "", fmt.Errorf("provider: %d nested containers exceed the limit of %d", len(inner)-1, lim.MaxDepth)
	}
	fsys, err := c.get(key, func() (*archiveFS, error) { return indexArchive(ctx, lim, src, format) })
	if err != nil {
		return nil, "", err
	}
	for i := 0; i+1 < len(inner); i++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		name, err := cleanMember(inner[i])
		if err != nil {
			return nil, "", err
		}
		e, ok := fsys.entries[name]
		if !ok {
			return nil, "", &fs.PathError{Op: "descend", Path: inner[i], Err: fs.ErrNotExist}
		}
		if !e.mode.IsRegular() {
			return nil, "", fmt.Errorf("provider: %s: cannot descend into a %s", inner[i], kindOf(e.mode))
		}
		next := ContainerFormat(name)
		if next == "" {
			return nil, "", fmt.Errorf("provider: %s: not a container (known suffixes: .zip .jar .tar .tgz .tar.gz .gz)", inner[i])
		}
		parent := fsys
		key += "!/" + name
		fsys, err = c.get(key, func() (*archiveFS, error) {
			data, err := parent.readBounded(name, lim.MaxBuffer)
			if err != nil {
				return nil, err
			}
			return indexArchive(ctx, lim, bytesBlob(path.Base(name), data), next)
		})
		if err != nil {
			return nil, "", err
		}
	}
	if len(inner) == 0 {
		return fsys, ".", nil
	}
	p, err := cleanMember(inner[len(inner)-1])
	return fsys, p, err
}

// cleanMember maps a member spelling to an fs.FS path. Leading slashes are
// dropped (as tar does); a path climbing out of the archive is refused.
func cleanMember(name string) (string, error) {
	p := path.Clean("/" + strings.TrimLeft(name, "/"))
	if strings.Contains("/"+strings.TrimLeft(name, "/")+"/", "/../") {
		return "", fmt.Errorf("provider: %q escapes the archive root", name)
	}
	if p == "/" {
		return ".", nil
	}
	return p[1:], nil
}

// archiveFS is a read-only, in-memory index over one container. It is an
// fs.FS (plus StatFS/ReadDirFS); member content is read from the source on
// Open, never held, except for nested containers (bounded by MaxBuffer).
type archiveFS struct {
	format  string
	name    string
	size    int64
	limits  Limits
	entries map[string]*archiveEntry
	skipped int
}

type archiveEntry struct {
	path     string
	mode     fs.FileMode
	size     int64 // -1 when unknown (gzip)
	mtime    time.Time
	link     string
	hardlink bool
	attrs    map[string]any
	children []string
	open     func() (io.ReadCloser, error)
}

func indexArchive(ctx context.Context, lim Limits, src blob, format string) (*archiveFS, error) {
	f := &archiveFS{format: format, name: src.name, size: src.size, limits: lim, entries: map[string]*archiveEntry{}}
	f.entries["."] = &archiveEntry{path: ".", mode: fs.ModeDir | 0o755, attrs: map[string]any{}}
	var err error
	switch format {
	case "tar":
		err = f.indexTar(ctx, src)
	case "zip":
		err = f.indexZip(ctx, src)
	case "gzip":
		err = f.indexGzip(src)
	default:
		err = fmt.Errorf("provider: unknown container format %q", format)
	}
	if err != nil {
		return nil, fmt.Errorf("provider: %s (%s): %w", src.name, format, err)
	}
	for _, e := range f.entries {
		if e.hardlink {
			if t, ok := f.entries[e.link]; ok && t.mode.IsRegular() && !t.hardlink {
				e.size, e.open = t.size, t.open
			}
		}
		sort.Strings(e.children)
	}
	return f, nil
}

func (f *archiveFS) add(name string, e *archiveEntry) error {
	p, err := cleanMember(name)
	if err != nil {
		f.skipped++
		return nil
	}
	if p == "." {
		return nil
	}
	if _, ok := f.entries[p]; !ok {
		if len(f.entries) > f.limits.MaxEntries {
			return fmt.Errorf("more than %d entries", f.limits.MaxEntries)
		}
		parent := path.Dir(p)
		if !f.dirPath(parent) {
			f.skipped++ // a member below a non-directory has no place in the tree
			return nil
		}
		f.ensureDir(parent)
		f.entries[parent].children = append(f.entries[parent].children, path.Base(p))
	} else if old := f.entries[p]; old.mode.IsDir() && e.mode.IsDir() {
		e.children = old.children // a later header for the same path wins, keeping its members
	}
	e.path = p
	if e.attrs == nil {
		e.attrs = map[string]any{}
	}
	f.entries[p] = e
	return nil
}

// dirPath reports whether p and its ancestors are, or can become, directories.
func (f *archiveFS) dirPath(p string) bool {
	for ; p != "."; p = path.Dir(p) {
		if e, ok := f.entries[p]; ok && !e.mode.IsDir() {
			return false
		}
	}
	return true
}

func (f *archiveFS) ensureDir(p string) {
	if _, ok := f.entries[p]; ok || p == "." {
		return
	}
	parent := path.Dir(p)
	f.ensureDir(parent)
	f.entries[parent].children = append(f.entries[parent].children, path.Base(p))
	f.entries[p] = &archiveEntry{path: p, mode: fs.ModeDir | 0o755, attrs: map[string]any{"implicit": true}}
}

// tarStream opens src as a tar stream, decompressing gzip by magic exactly as
// yoke tar's read path does.
func tarStream(src blob) (*tar.Reader, io.Closer, error) {
	rc, err := src.open()
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(rc)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			rc.Close()
			return nil, nil, err
		}
		return tar.NewReader(zr), rc, nil
	}
	return tar.NewReader(br), rc, nil
}

func (f *archiveFS) indexTar(ctx context.Context, src blob) error {
	tr, closer, err := tarStream(src)
	if err != nil {
		return err
	}
	defer closer.Close()
	for ord := 0; ; ord++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		e := &archiveEntry{size: hdr.Size, mtime: hdr.ModTime, attrs: map[string]any{"typeflag": string(rune(hdr.Typeflag))}}
		perm := fs.FileMode(hdr.Mode) & fs.ModePerm
		switch hdr.Typeflag {
		case tar.TypeDir:
			e.mode = fs.ModeDir | perm
		case tar.TypeSymlink:
			e.mode, e.link, e.size = fs.ModeSymlink|perm, hdr.Linkname, int64(len(hdr.Linkname))
		case tar.TypeLink:
			e.mode, e.hardlink, e.size = perm, true, 0
			if e.link, err = cleanMember(hdr.Linkname); err != nil {
				e.link = ""
			}
		case tar.TypeChar:
			e.mode = fs.ModeDevice | fs.ModeCharDevice | perm
		case tar.TypeBlock:
			e.mode = fs.ModeDevice | perm
		case tar.TypeFifo:
			e.mode = fs.ModeNamedPipe | perm
		default:
			e.mode = perm
		}
		if hdr.Linkname != "" {
			e.attrs["linkname"] = hdr.Linkname
		}
		if hdr.Uname != "" {
			e.attrs["uname"] = hdr.Uname
		}
		if hdr.Gname != "" {
			e.attrs["gname"] = hdr.Gname
		}
		e.attrs["uid"], e.attrs["gid"] = hdr.Uid, hdr.Gid
		if e.mode.IsRegular() && !e.hardlink {
			e.open = tarMemberOpener(src, ord)
		} else if e.mode&fs.ModeSymlink != 0 {
			e.open = linkOpener(e.link)
		}
		if err := f.add(hdr.Name, e); err != nil {
			return err
		}
	}
}

// tarMemberOpener rescans the stream to header ord: a tar member has no
// random access, and holding contents would make indexing unbounded.
func tarMemberOpener(src blob, ord int) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		tr, closer, err := tarStream(src)
		if err != nil {
			return nil, err
		}
		for i := 0; ; i++ {
			if _, err := tr.Next(); err != nil && !errors.Is(err, tar.ErrInsecurePath) {
				closer.Close()
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
			if i == ord {
				return struct {
					io.Reader
					io.Closer
				}{tr, closer}, nil
			}
		}
	}
}

func linkOpener(target string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(target)), nil }
}

func (f *archiveFS) indexZip(ctx context.Context, src blob) error {
	at, closer, err := src.at()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(at, src.size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		closer.Close()
		return err
	}
	// The ReaderAt stays open for member reads; zip has no close hook, so an
	// os.File source is reopened per member instead of kept.
	closer.Close()
	for i, zf := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &archiveEntry{size: int64(zf.UncompressedSize64), mtime: zf.Modified, mode: zf.Mode(), attrs: map[string]any{
			"method": zipMethod(zf.Method), "crc32": fmt.Sprintf("%08x", zf.CRC32), "compressed_size": zf.CompressedSize64,
		}}
		if zf.Comment != "" {
			e.attrs["comment"] = zf.Comment
		}
		if strings.HasSuffix(zf.Name, "/") {
			e.mode = fs.ModeDir | e.mode.Perm()
		}
		if !e.mode.IsDir() {
			e.open = zipMemberOpener(src, i)
		}
		if err := f.add(zf.Name, e); err != nil {
			return err
		}
	}
	return nil
}

func zipMemberOpener(src blob, i int) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		at, closer, err := src.at()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(at, src.size)
		if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
			closer.Close()
			return nil, err
		}
		if i >= len(zr.File) {
			closer.Close()
			return nil, io.ErrUnexpectedEOF
		}
		rc, err := zr.File[i].Open()
		if err != nil {
			closer.Close()
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{rc, multiCloser{rc, closer}}, nil
	}
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func zipMethod(m uint16) string {
	switch m {
	case zip.Store:
		return "store"
	case zip.Deflate:
		return "deflate"
	}
	return fmt.Sprintf("%d", m)
}

// indexGzip exposes the one decompressed stream as a single member named as
// gunzip would name its output (the header name, else the source name
// without .gz; .tgz becomes .tar).
func (f *archiveFS) indexGzip(src blob) error {
	rc, err := src.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	hdr := zr.Header
	name := path.Base(strings.ReplaceAll(hdr.Name, "\\", "/"))
	if hdr.Name == "" || name == "." || name == "/" || name == ".." {
		lower := strings.ToLower(src.name)
		switch {
		case strings.HasSuffix(lower, ".tgz"):
			name = src.name[:len(src.name)-4] + ".tar"
		case strings.HasSuffix(lower, ".gz") && len(src.name) > 3:
			name = src.name[:len(src.name)-3]
		default:
			name = src.name + ".out"
		}
	}
	e := &archiveEntry{size: -1, mtime: hdr.ModTime, mode: 0o644, attrs: map[string]any{"os": int(hdr.OS)}}
	if hdr.Name != "" {
		e.attrs["header_name"] = hdr.Name
	}
	if hdr.Comment != "" {
		e.attrs["comment"] = hdr.Comment
	}
	e.open = func() (io.ReadCloser, error) {
		rc, err := src.open()
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(rc)
		if err != nil {
			rc.Close()
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{zr, multiCloser{zr, rc}}, nil
	}
	return f.add(name, e)
}

// limitedReader fails loudly once a member yields more than max bytes,
// rather than truncating silently.
type limitedReader struct {
	rc  io.ReadCloser
	max int64
	n   int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.rc.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		return n - int(l.n-l.max), fmt.Errorf("provider: member exceeds the %d byte limit", l.max)
	}
	return n, err
}
func (l *limitedReader) Close() error { return l.rc.Close() }

// openMember opens p's content, bounded by MaxMember. A directory is an
// error; a symlink yields its target spelling and is never followed.
func (f *archiveFS) openMember(p string) (io.ReadCloser, error) {
	e, ok := f.entries[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if e.mode.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: errors.New("is a directory")}
	}
	if e.open == nil {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fmt.Errorf("%s has no content", kindOf(e.mode))}
	}
	rc, err := e.open()
	if err != nil {
		return nil, err
	}
	return &limitedReader{rc: rc, max: f.limits.MaxMember}, nil
}

func (f *archiveFS) readBounded(p string, max int64) ([]byte, error) {
	rc, err := f.openMember(p)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("provider: %s: nested container exceeds the %d byte buffer limit", p, max)
	}
	return data, nil
}

func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	}
	return "special"
}

func typeOf(m fs.FileMode) string {
	switch kindOf(m) {
	case "directory":
		return "inode/directory"
	case "symlink":
		return "inode/symlink"
	case "special":
		return "inode/x-special"
	}
	return "application/octet-stream"
}

// withInner is loc addressing member p of its innermost container.
func withInner(loc ref.Locator, p string) ref.Locator {
	inner := append([]string(nil), loc.Inner...)
	if len(inner) == 0 {
		inner = append(inner, p)
	} else {
		inner[len(inner)-1] = p
	}
	loc.Inner = inner
	return loc
}

func (f *archiveFS) node(loc ref.Locator, p string) (Node, error) {
	e, ok := f.entries[p]
	if !ok {
		return Node{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	n := Node{SchemaVersion: NodeSchema, Ref: loc.String(), Path: p, Name: path.Base(p), Kind: kindOf(e.mode), Type: typeOf(e.mode), Mode: e.mode.String(), Attrs: map[string]any{"format": f.format}}
	for k, v := range e.attrs {
		n.Attrs[k] = v
	}
	if p == "." {
		n.Path, n.Name = "", f.name
		n.Attrs["entries"] = len(f.entries) - 1
		n.Attrs["source_size"] = f.size
		if f.skipped > 0 {
			n.Attrs["skipped_unsafe"] = f.skipped
		}
	}
	if e.size >= 0 && !e.mode.IsDir() {
		size := e.size
		n.Size = &size
	}
	if !e.mtime.IsZero() {
		n.Mtime = e.mtime.UTC().Format(time.RFC3339Nano)
	}
	if e.mode.IsDir() {
		c := uint64(len(e.children))
		n.Children = &c
	} else if c := ContainerFormat(p); c != "" && e.mode.IsRegular() {
		n.Attrs["container"] = c // descend with !/
	}
	return n, nil
}

func (f *archiveFS) list(ctx context.Context, loc ref.Locator, p string) ([]Node, error) {
	e, ok := f.entries[p]
	if !ok {
		return nil, &fs.PathError{Op: "list", Path: p, Err: fs.ErrNotExist}
	}
	if !e.mode.IsDir() {
		return nil, &fs.PathError{Op: "list", Path: p, Err: errors.New("not a directory")}
	}
	out := make([]Node, 0, len(e.children))
	for _, name := range e.children {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child := path.Join(p, name)
		n, err := f.node(withInner(loc, child), child)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// fs.FS surface — lets any io/fs consumer (fs.WalkDir, fstest) read a container.

func (f *archiveFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	e, ok := f.entries[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if e.mode.IsDir() {
		return &archiveDir{fsys: f, e: e}, nil
	}
	rc, err := f.openMember(name)
	if err != nil {
		return nil, err
	}
	return &archiveFile{rc: rc, e: e}, nil
}

func (f *archiveFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	e, ok := f.entries[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return entryInfo{e}, nil
}

func (f *archiveFS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	d, ok := file.(*archiveDir)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return d.ReadDir(-1)
}

type entryInfo struct{ e *archiveEntry }

func (i entryInfo) Name() string {
	if i.e.path == "." {
		return "."
	}
	return path.Base(i.e.path)
}
func (i entryInfo) Size() int64 {
	if i.e.size < 0 || i.e.mode.IsDir() {
		return 0
	}
	return i.e.size
}
func (i entryInfo) Mode() fs.FileMode          { return i.e.mode }
func (i entryInfo) ModTime() time.Time         { return i.e.mtime }
func (i entryInfo) IsDir() bool                { return i.e.mode.IsDir() }
func (i entryInfo) Sys() any                   { return nil }
func (i entryInfo) Type() fs.FileMode          { return i.e.mode.Type() }
func (i entryInfo) Info() (fs.FileInfo, error) { return i, nil }
func (i entryInfo) String() string             { return fs.FormatFileInfo(i) }

type archiveFile struct {
	rc io.ReadCloser
	e  *archiveEntry
}

func (f *archiveFile) Read(p []byte) (int, error) { return f.rc.Read(p) }
func (f *archiveFile) Close() error               { return f.rc.Close() }
func (f *archiveFile) Stat() (fs.FileInfo, error) { return entryInfo{f.e}, nil }

type archiveDir struct {
	fsys *archiveFS
	e    *archiveEntry
	off  int
}

func (d *archiveDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.e.path, Err: errors.New("is a directory")}
}
func (d *archiveDir) Close() error               { return nil }
func (d *archiveDir) Stat() (fs.FileInfo, error) { return entryInfo{d.e}, nil }
func (d *archiveDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.e.children[d.off:]
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	out := make([]fs.DirEntry, 0, len(rest))
	for _, name := range rest {
		out = append(out, entryInfo{d.fsys.entries[path.Join(d.e.path, name)]})
	}
	d.off += len(rest)
	return out, nil
}
