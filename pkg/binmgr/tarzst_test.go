package binmgr

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// The official linux ollama ships as .tar.zst: tree and member extraction
// both read it, and a GitHub asset with that suffix is usable for a tree.
func TestTarZstExtraction(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, f := range []struct{ name, body string }{{"bin/ollama", "#!/bin/sh\necho ok\n"}, {"lib/ollama/libx.so", "x"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f.body))
	}
	tw.Close()
	var zst bytes.Buffer
	enc, _ := zstd.NewWriter(&zst)
	enc.Write(raw.Bytes())
	enc.Close()
	dir := t.TempDir()
	archive := filepath.Join(dir, "ollama-linux-amd64.tar.zst")
	if err := os.WriteFile(archive, zst.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := extractTree(archive, "https://x/ollama-linux-amd64.tar.zst", tree); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(tree, "bin", "ollama")); err != nil || string(data) != "#!/bin/sh\necho ok\n" {
		t.Fatalf("tree member: %q %v", data, err)
	}
	one := filepath.Join(dir, "one")
	if err := extract(archive, "https://x/ollama-linux-amd64.tar.zst", "lib/ollama/libx.so", one); err != nil {
		t.Fatal(err)
	}
	if !assetUsable("ollama-linux-amd64.tar.zst", "", true) {
		t.Fatal(".tar.zst asset not usable for a tree")
	}
}
