package binmgr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func memberArchive(t *testing.T, names []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		payload := []byte(name + " bytes")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestEnsureMembersOneDownloadAndAtomicConcurrentPublish(t *testing.T) {
	names := []string{BinaryName("bashy"), BinaryName("outpost"), BinaryName("bash"), BinaryName("sh")}
	body := memberArchive(t, names)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write(body) }))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := toolFor(srv.URL+"/bashy.tar.gz", sha256hex(body), names[0])
	if _, err := EnsureArchive(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths, err := EnsureMembers(context.Background(), tool, names)
			if err != nil {
				t.Error(err)
				return
			}
			for _, name := range names {
				data, err := os.ReadFile(paths[name])
				if err != nil || string(data) != name+" bytes" {
					t.Errorf("member %s: %q %v", name, data, err)
				}
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("archive downloads=%d", hits.Load())
	}
}
func TestExtractMembersRejectsMissingDuplicateAndTraversal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
	}{{"missing", []string{"bashy"}}, {"duplicate", []string{"bashy", "bashy", "outpost"}}, {"traversal", []string{"bashy", "outpost", "../escape"}}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(archive, memberArchive(t, tc.entries), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ExtractMembers(archive, filepath.Join(dir, "stage"), []string{"bashy", "outpost"}); err == nil {
				t.Fatal("expected invalid archive error")
			}
		})
	}
}
func TestExtractMembersZipAndRejectSymlink(t *testing.T) {
	for _, link := range []bool{false, true} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: "bashy.exe"}
		h.SetMode(0755)
		if link {
			h.SetMode(os.ModeSymlink | 0755)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("payload"))
		zw.Close()
		dir := t.TempDir()
		archive := filepath.Join(dir, "release.zip")
		os.WriteFile(archive, buf.Bytes(), 0600)
		_, err = ExtractMembers(archive, filepath.Join(dir, "stage"), []string{"bashy.exe"})
		if link && err == nil {
			t.Fatal("symlink accepted")
		}
		if !link && err != nil {
			t.Fatal(err)
		}
	}
}

func TestCachedBinaryDiscoversFetchedProductAndPreservesNewestSelection(t *testing.T) {
	names := []string{BinaryName("bashy"), BinaryName("outpost"), BinaryName("bash"), BinaryName("sh")}
	body := memberArchive(t, names)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	tool := toolFor(srv.URL+"/bashy.tar.gz", sha256hex(body), names[0])
	tool.Name = "bashy"
	paths, err := EnsureMembers(context.Background(), tool, names)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close() // discovery is entirely local after the one product fetch.
	older := time.Unix(10, 0)
	newer := time.Unix(20, 0)
	for _, name := range []string{"bashy", "outpost", "bash", "sh"} {
		paired := paths[BinaryName(name)]
		if err := os.Chtimes(paired, older, older); err != nil {
			t.Fatal(err)
		}
		if got := CachedBinary(name); got != paired {
			t.Fatalf("%s cache=%q; want fetched member %q", name, got, paired)
		}
		standalone := filepath.Join(root, name, "standalone", BinaryName(name))
		if err := os.MkdirAll(filepath.Dir(standalone), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(standalone, []byte("standalone"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(standalone, newer, newer); err != nil {
			t.Fatal(err)
		}
		if got := CachedBinary(name); got != standalone {
			t.Fatalf("newest %s cache=%q; want %q", name, got, standalone)
		}
		if err := os.Chtimes(paired, newer.Add(time.Second), newer.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if got := CachedBinary(name); got != paired {
			t.Fatalf("newest pair %s cache=%q; want %q", name, got, paired)
		}
	}
}
