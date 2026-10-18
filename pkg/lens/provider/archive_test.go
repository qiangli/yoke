package provider

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

var fixedTime = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type member struct {
	name, body, link string
	flag             byte
}

func tarBytes(t *testing.T, gz bool, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, m := range ms {
		flag := m.flag
		if flag == 0 {
			flag = tar.TypeReg
		}
		h := &tar.Header{Name: m.name, Typeflag: flag, Mode: 0o644, ModTime: fixedTime, Linkname: m.link, Uname: "u"}
		if flag == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		if flag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			tw.Write([]byte(m.body))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range ms {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate, Modified: fixedTime}
		h.SetMode(0o644)
		if strings.HasSuffix(m.name, "/") {
			h.SetMode(fs.ModeDir | 0o755)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(m.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(name, body string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Name, zw.ModTime = name, fixedTime
	zw.Write([]byte(body))
	zw.Close()
	return buf.Bytes()
}

// fixture: release.zip holds docs/, a nested tgz and a nested gz; the tgz
// holds a dir, a symlink, a hardlink and unsafe names that must not surface.
func archiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	inner := tarBytes(t, true,
		member{name: "pkg/", flag: tar.TypeDir},
		member{name: "pkg/inner.txt", body: "inner body"},
		member{name: "pkg/link", link: "inner.txt", flag: tar.TypeSymlink},
		member{name: "pkg/hard", link: "pkg/inner.txt", flag: tar.TypeLink},
		member{name: "../evil.txt", body: "no"},
		member{name: "/abs/ok.txt", body: "abs"},
		member{name: "implicit/dir/f.txt", body: "f"},
	)
	outer := zipBytes(t,
		member{name: "docs/"},
		member{name: "docs/readme.md", body: "# hi\n"},
		member{name: "nested.tgz", body: string(inner)},
		member{name: "notes.txt.gz", body: string(gzipBytes("notes.txt", "gz body"))},
	)
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("release.zip", outer)
	write("inner.tgz", inner)
	write("plain.gz", gzipBytes("", "plain"))
	return dir
}

func loc(t *testing.T, s string) ref.Locator {
	t.Helper()
	l, err := ref.ParseLocator(s)
	if err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return l
}

func readAll(t *testing.T, p Provider, s string) string {
	t.Helper()
	rc, err := p.Open(context.Background(), loc(t, s))
	if err != nil {
		t.Fatalf("open %s: %v", s, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", s, err)
	}
	return string(b)
}

func TestArchiveFSConformance(t *testing.T) {
	dir := archiveFixture(t)
	for _, tc := range []struct {
		file, format string
		want         []string
	}{
		{"inner.tgz", "tar", []string{"pkg/inner.txt", "pkg/hard", "abs/ok.txt", "implicit/dir/f.txt"}},
		{"release.zip", "zip", []string{"docs/readme.md", "nested.tgz", "notes.txt.gz"}},
		{"plain.gz", "gzip", []string{"plain"}},
	} {
		a := NewArchive(dir)
		src, err := a.source(ref.Locator{Scheme: tc.format, Target: tc.file})
		if err != nil {
			t.Fatal(err)
		}
		fsys, err := indexArchive(context.Background(), DefaultLimits, src, tc.format)
		if err != nil {
			t.Fatal(err)
		}
		if err := fstest.TestFS(fsys, tc.want...); err != nil {
			t.Errorf("%s: %v", tc.file, err)
		}
	}
}

func TestArchiveProviderRows(t *testing.T) {
	ctx := context.Background()
	a := NewArchive(archiveFixture(t))

	root, err := a.Stat(ctx, loc(t, "zip:release.zip"))
	if err != nil || root.Kind != "directory" || root.Path != "" || root.Name != "release.zip" || root.Children == nil || *root.Children != 3 {
		t.Fatalf("root %+v %v", root, err)
	}
	rows, err := a.List(ctx, loc(t, "zip:release.zip"))
	if err != nil || len(rows) != 3 {
		t.Fatalf("list %+v %v", rows, err)
	}
	want := []string{"zip:release.zip!/docs", "zip:release.zip!/nested.tgz", "zip:release.zip!/notes.txt.gz"}
	for i, r := range rows {
		if r.Ref != want[i] || r.SchemaVersion != NodeSchema || r.Attrs["format"] != "zip" {
			t.Fatalf("row %d: %+v", i, r)
		}
		if _, err := ref.ParseLocator(r.Ref); err != nil {
			t.Fatalf("ref %s does not round-trip: %v", r.Ref, err)
		}
	}
	if rows[1].Attrs["container"] != "tar" || rows[2].Attrs["container"] != "gzip" {
		t.Fatalf("container hints: %+v %+v", rows[1].Attrs, rows[2].Attrs)
	}
	// `!/.` is the container root, same as no inner path.
	if dot, _ := a.Stat(ctx, loc(t, "zip:release.zip!/.")); dot.Kind != "directory" || *dot.Children != 3 {
		t.Fatalf("dot root %+v", dot)
	}

	if got := readAll(t, a, "zip:release.zip!/docs/readme.md"); got != "# hi\n" {
		t.Fatalf("zip member %q", got)
	}
	// Chained descent: zip → tgz → member, and zip → gz → member.
	if got := readAll(t, a, "zip:release.zip!/nested.tgz!/pkg/inner.txt"); got != "inner body" {
		t.Fatalf("chained %q", got)
	}
	if got := readAll(t, a, "zip:release.zip!/notes.txt.gz!/notes.txt"); got != "gz body" {
		t.Fatalf("chained gz %q", got)
	}
	nested, err := a.List(ctx, loc(t, "zip:release.zip!/nested.tgz!/pkg"))
	if err != nil || len(nested) != 3 || nested[0].Ref != "zip:release.zip!/nested.tgz!/pkg/hard" {
		t.Fatalf("nested list %+v %v", nested, err)
	}
	// tar: hardlink reads its target; a symlink yields its target spelling, unfollowed.
	if got := readAll(t, a, "tar:inner.tgz!/pkg/hard"); got != "inner body" {
		t.Fatalf("hardlink %q", got)
	}
	if got := readAll(t, a, "tar:inner.tgz!/pkg/link"); got != "inner.txt" {
		t.Fatalf("symlink %q", got)
	}
	if n, _ := a.Stat(ctx, loc(t, "tar:inner.tgz!/pkg/link")); n.Kind != "symlink" || n.Attrs["linkname"] != "inner.txt" {
		t.Fatalf("symlink row %+v", n)
	}
	// Unsafe names never surface; the root counts them.
	troot, _ := a.Stat(ctx, loc(t, "tar:inner.tgz"))
	if troot.Attrs["skipped_unsafe"] != 1 {
		t.Fatalf("skipped %+v", troot.Attrs)
	}
	if _, err := a.Stat(ctx, loc(t, "tar:inner.tgz!/evil.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("evil: %v", err)
	}
	if _, err := a.Stat(ctx, loc(t, "tar:inner.tgz!/pkg/../../evil.txt")); err == nil {
		t.Fatal("climbing path accepted")
	}
	// gzip: one member, named as gunzip would; size unknown, not zero.
	g, err := a.List(ctx, loc(t, "gzip:plain.gz"))
	if err != nil || len(g) != 1 || g[0].Name != "plain" || g[0].Size != nil {
		t.Fatalf("gzip %+v %v", g, err)
	}
	edges, err := a.Edges(ctx, loc(t, "zip:release.zip"))
	if err != nil || len(edges) != 3 || edges[0].Rel != "contains" || edges[0].Src != "zip:release.zip" {
		t.Fatalf("edges %+v %v", edges, err)
	}
}

func TestArchiveErrorsAndBounds(t *testing.T) {
	ctx := context.Background()
	dir := archiveFixture(t)
	a := NewArchive(dir)
	for s, frag := range map[string]string{
		"zip:release.zip!/docs/readme.md!/x": "not a container",
		"zip:release.zip!/docs!/x":           "cannot descend",
		"zip:release.zip!/missing.tgz!/x":    "file does not exist",
		"zip:missing.zip":                    "no such file",
		"zip:release.zip@ws:abc":             "unsupported",
	} {
		if _, err := a.Stat(ctx, loc(t, s)); err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("%s: %v (want %q)", s, err, frag)
		}
	}
	if _, err := a.Open(ctx, loc(t, "zip:release.zip!/docs")); err == nil {
		t.Error("opened a directory")
	}

	small := func(mod func(*Limits)) *Archive {
		b := NewArchive(dir)
		mod(&b.Limits)
		return b
	}
	if _, err := small(func(l *Limits) { l.MaxEntries = 2 }).Stat(ctx, loc(t, "tar:inner.tgz")); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("MaxEntries: %v", err)
	}
	if _, err := small(func(l *Limits) { l.MaxBuffer = 16 }).Stat(ctx, loc(t, "zip:release.zip!/nested.tgz!/.")); err == nil || !strings.Contains(err.Error(), "buffer limit") {
		t.Errorf("MaxBuffer: %v", err)
	}
	if _, err := small(func(l *Limits) { l.MaxDepth = 0 }).Stat(ctx, loc(t, "zip:release.zip!/nested.tgz!/.")); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Errorf("MaxDepth: %v", err)
	}
	rc, err := small(func(l *Limits) { l.MaxMember = 4 }).Open(ctx, loc(t, "tar:inner.tgz!/pkg/inner.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(rc); err == nil || len(b) > 4 {
		t.Errorf("MaxMember: %q %v", b, err)
	}
	rc.Close()

	// Read-only: no operation touched the source directory.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("source dir changed: %v", entries)
	}
}
