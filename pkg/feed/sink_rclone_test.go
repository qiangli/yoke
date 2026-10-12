package feed

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/rclone"
)

func TestRcloneSinkLocalPath(t *testing.T) {
	ctx := context.Background()
	if _, err := rclone.Path(ctx, ""); err != nil {
		t.Skipf("rclone binary does not resolve and there is no network to fetch it: %v", err)
	}
	root := t.TempDir()
	target := filepath.Join(root, "remote")

	s, err := OpenSink(SinkConfig{Name: "r1", Kind: "rclone", Target: target})
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	if s.Kind() != "rclone" || s.Name() != "r1" {
		t.Fatalf("sink = %q/%q", s.Kind(), s.Name())
	}

	data := []byte(`{"headline":"hi"}`)
	url, err := s.Put(ctx, "posts/a.json", data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Local remotes do not support `rclone link`, so Put must fall back to
	// the destination path as the URL rather than fail. (If a future rclone
	// does link local files, any non-empty URL is fine.)
	dest := target + "/posts/a.json"
	if url != dest && strings.TrimSpace(url) == "" {
		t.Fatalf("Put URL = %q", url)
	}

	onDisk, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(onDisk, data) {
		t.Fatalf("on-disk content = %q,%v", onDisk, err)
	}

	// Get via relPath always works.
	back, err := s.Get(ctx, "posts/a.json")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("Get relPath = %q,%v", back, err)
	}
	// Get via the returned URL works when it is the dest-path fallback.
	if url == dest {
		back, err := s.Get(ctx, url)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("Get URL = %q,%v", back, err)
		}
	}

	// Overwrite publishes the new content.
	data2 := []byte(`{"headline":"hi2"}`)
	if _, err := s.Put(ctx, "posts/a.json", data2); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	back, err = s.Get(ctx, "posts/a.json")
	if err != nil || !bytes.Equal(back, data2) {
		t.Fatalf("Get after overwrite = %q,%v", back, err)
	}
}

func TestRcloneSinkGetMissing(t *testing.T) {
	ctx := context.Background()
	if _, err := rclone.Path(ctx, ""); err != nil {
		t.Skipf("rclone binary does not resolve and there is no network to fetch it: %v", err)
	}
	s, err := OpenSink(SinkConfig{Name: "r1", Kind: "rclone", Target: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "posts/nope.json"); err == nil {
		t.Fatal("Get missing: nil error")
	}
}

func TestRcloneSinkRejectsTraversal(t *testing.T) {
	s, err := OpenSink(SinkConfig{Name: "r1", Kind: "rclone", Target: "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "../evil", []byte("x")); err == nil {
		t.Fatal("Put ../evil: nil error")
	}
}
