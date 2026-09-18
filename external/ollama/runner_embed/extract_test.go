package runner_embed

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestAvailable_Stub(t *testing.T) {
	// Without embed_runner tag, runner should not be available.
	if Available() {
		t.Skip("runner is embedded (built with -tags embed_runner)")
	}
	t.Log("runner not embedded (stub mode) — expected for dev builds")
}

func TestEnsureRunner_NoEmbed(t *testing.T) {
	if Available() {
		t.Skip("runner is embedded")
	}
	_, err := EnsureRunner(t.TempDir())
	if err == nil {
		t.Error("expected error when no runner is embedded")
	}
}

func TestEnsureRunner_WithFakeRunner(t *testing.T) {
	// Simulate an embedded runner by setting compressedRunner directly.
	fakeScript := []byte("#!/bin/sh\necho hello\n")

	var buf []byte
	w := gzipCompress(fakeScript)
	buf = w

	old := compressedRunner
	compressedRunner = buf
	defer func() { compressedRunner = old }()

	dir := t.TempDir()

	// First extraction.
	path, err := EnsureRunner(dir)
	if err != nil {
		t.Fatalf("first extraction: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("runner not found at %s: %v", path, err)
	}
	info, _ := os.Stat(path)
	if info.Mode()&0o111 == 0 {
		t.Error("runner should be executable")
	}

	// Second call should be cached (no re-extraction).
	path2, err := EnsureRunner(dir)
	if err != nil {
		t.Fatalf("cached extraction: %v", err)
	}
	if path2 != path {
		t.Errorf("expected same path, got %s vs %s", path, path2)
	}

	// Verify hash file exists.
	hashPath := path + ".sha256"
	if _, err := os.Stat(hashPath); err != nil {
		t.Error("hash file not created")
	}

	// Modify the embedded data — should re-extract.
	newScript := []byte("#!/bin/sh\necho updated\n")
	compressedRunner = gzipCompress(newScript)

	path3, err := EnsureRunner(dir)
	if err != nil {
		t.Fatalf("re-extraction after update: %v", err)
	}

	content, _ := os.ReadFile(path3)
	if string(content) != string(newScript) {
		t.Error("runner was not re-extracted after hash change")
	}
}

func TestEnsureRunner_AtomicWrite(t *testing.T) {
	// Verify no .tmp file is left behind on success.
	fakeScript := []byte("#!/bin/sh\necho atomic\n")
	old := compressedRunner
	compressedRunner = gzipCompress(fakeScript)
	defer func() { compressedRunner = old }()

	dir := t.TempDir()
	path, err := EnsureRunner(dir)
	if err != nil {
		t.Fatal(err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Error(".tmp file should not exist after successful extraction")
	}
}

// gzipCompress compresses data with gzip.
func gzipCompress(data []byte) []byte {
	var buf []byte
	var b = new(bytesBuffer)
	w, _ := gzip.NewWriterLevel(b, gzip.BestCompression)
	w.Write(data)
	w.Close()
	buf = b.Bytes()
	return buf
}

// bytesBuffer is a simple bytes.Buffer replacement to avoid import.
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte {
	return b.data
}

func TestRunnerName(t *testing.T) {
	expected := "ycode-runner"
	if runnerName != expected {
		t.Errorf("runnerName = %q, want %q", runnerName, expected)
	}
}

func TestCodesign(t *testing.T) {
	// Just verify it doesn't panic on a non-existent file.
	codesign(filepath.Join(t.TempDir(), "nonexistent"))
}
