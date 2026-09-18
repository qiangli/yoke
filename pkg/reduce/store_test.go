// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStorePutGetRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("the complete output\nwith several lines\n")
	digest, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest lacks scheme: %s", digest)
	}
	got, gotDigest, err := s.Get(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("round-trip mismatch")
	}
	if gotDigest != digest {
		t.Fatalf("digest mismatch: %s vs %s", gotDigest, digest)
	}
}

func TestStorePutIdempotent(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("same bytes")
	d1, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Put(content)
	if err != nil {
		t.Fatalf("second identical Put should be a no-op: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("content address changed: %s vs %s", d1, d2)
	}
	// Only one blob on disk.
	entries, _ := os.ReadDir(s.Root())
	n := 0
	for _, e := range entries {
		if len(e.Name()) == digestHexLen {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected 1 blob, found %d", n)
	}
}

func TestStorePutConcurrentIdentical(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("concurrent identical bytes")
	const workers = 32
	results := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digest, err := s.Put(content)
			results <- digest
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	var want string
	for digest := range results {
		if want == "" {
			want = digest
		}
		if digest != want {
			t.Fatalf("concurrent Put returned %q and %q", want, digest)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreGetByShortHandle(t *testing.T) {
	s := NewStore(t.TempDir())
	digest, err := s.Put([]byte("recoverable"))
	if err != nil {
		t.Fatal(err)
	}
	hexsum := strings.TrimPrefix(digest, "sha256:")
	handle := hexsum[:MinHandleLen]
	got, _, err := s.Get(handle)
	if err != nil {
		t.Fatalf("short handle should resolve: %v", err)
	}
	if string(got) != "recoverable" {
		t.Fatalf("wrong content for short handle")
	}
	// A bare hex prefix with the scheme should work too.
	if _, _, err := s.Get("sha256:" + handle); err != nil {
		t.Fatalf("scheme-prefixed handle should resolve: %v", err)
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, _, err := s.Get("deadbeef"); err == nil {
		t.Fatalf("missing handle must error")
	}
	if _, _, err := s.Get("nothex!!"); err == nil {
		t.Fatalf("non-hex handle must error")
	}
	if _, _, err := s.Get(""); err == nil {
		t.Fatalf("empty handle must error")
	}
}

func TestStoreAmbiguousHandle(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	// Two blobs whose names share an 8-char prefix. Real sha256 collisions do
	// not occur, so craft the filenames directly to exercise the ambiguity path.
	prefix := strings.Repeat("a", 8)
	for _, suffix := range []string{"1", "2"} {
		name := prefix + strings.Repeat("0", digestHexLen-len(prefix)-1) + suffix
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Get(prefix); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestStorePutNoRoot(t *testing.T) {
	var s *Store
	if _, err := s.Put([]byte("x")); err == nil {
		t.Fatalf("nil store must error on Put")
	}
	empty := NewStore("")
	if _, err := empty.Put([]byte("x")); err == nil {
		t.Fatalf("rootless store must error on Put")
	}
}

func TestRecoverViaOutCmd(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("FAIL: something broke\nstack trace line\n")
	digest, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	handle := strings.TrimPrefix(digest, "sha256:")[:MinHandleLen]

	cmd := NewOutCmd(func() (*Store, error) { return s, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{handle})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), content) {
		t.Fatalf("out verb did not reprint exact bytes")
	}
}
