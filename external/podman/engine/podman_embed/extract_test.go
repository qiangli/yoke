package podman_embed

import (
	"bytes"
	"compress/gzip"
	"os"
	"testing"
)

func TestAvailable_Stub(t *testing.T) {
	if Available() {
		t.Skip("podman is embedded (built with -tags embed_podman)")
	}
	t.Log("podman not embedded (stub mode) — expected for dev builds")
}

func TestEnsurePodman_NoEmbed(t *testing.T) {
	if Available() {
		t.Skip("podman is embedded")
	}
	_, err := EnsurePodman(t.TempDir())
	if err == nil {
		t.Error("expected error when no podman is embedded")
	}
}

func TestEnsurePodman_WithFake(t *testing.T) {
	fake := gzipBytes([]byte("#!/bin/sh\necho podman\n"))
	old := compressedPodman
	compressedPodman = fake
	defer func() { compressedPodman = old }()

	dir := t.TempDir()

	// First extraction.
	path, err := EnsurePodman(dir)
	if err != nil {
		t.Fatalf("first extraction: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("binary not found at %s", path)
	}
	info, _ := os.Stat(path)
	if info.Mode()&0o111 == 0 {
		t.Error("binary should be executable")
	}

	// Second call should be cached.
	path2, err := EnsurePodman(dir)
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if path2 != path {
		t.Errorf("paths differ: %s vs %s", path, path2)
	}

	// Hash change → re-extract.
	compressedPodman = gzipBytes([]byte("#!/bin/sh\necho updated\n"))
	path3, err := EnsurePodman(dir)
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	content, _ := os.ReadFile(path3)
	if !bytes.Contains(content, []byte("updated")) {
		t.Error("binary not re-extracted after hash change")
	}
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}
