package feed

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKeyDirUsesBashyHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	if got, want := KeyDir(), filepath.Join(home, "feed"); got != want {
		t.Fatalf("KeyDir() = %q, want %q", got, want)
	}
}

func TestKeyDirEmptyBashyHomeFallsBackToHome(t *testing.T) {
	t.Setenv("BASHY_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	if got, want := KeyDir(), filepath.Join(home, ".bashy", "feed"); got != want {
		t.Fatalf("KeyDir() = %q, want %q", got, want)
	}
}

func TestLoadOrCreateKeyStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "feed")
	k1, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	k2, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("second LoadOrCreateKey returned a different key")
	}

	path := filepath.Join(dir, keyFileName)
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("key file holds %d bytes, want %d-byte raw seed", len(seed), ed25519.SeedSize)
	}
	if got, want := ed25519.NewKeyFromSeed(seed), k1; !bytes.Equal(got, want) {
		t.Fatal("key derived from stored seed differs from returned key")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read key dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != keyFileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("key dir contains %v, want only %q (temp file leaked?)", names, keyFileName)
	}

	if runtime.GOOS == "windows" {
		return
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat key dir: %v", err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key dir mode %04o leaks group/other bits", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %04o, want 0600", fileInfo.Mode().Perm())
	}
}

func TestLoadOrCreateKeyLoadsExistingSeed(t *testing.T) {
	dir := t.TempDir()
	_, want, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	path := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(path, want.Seed(), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	got, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("LoadOrCreateKey did not return the stored key")
	}
}

func TestLoadOrCreateKeyRejectsBadSeedSize(t *testing.T) {
	for _, size := range []int{0, 1, 31, 33, 64} {
		dir := t.TempDir()
		path := filepath.Join(dir, keyFileName)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write seed: %v", err)
		}
		if _, err := LoadOrCreateKey(dir); err == nil {
			t.Fatalf("%d-byte key file accepted, want error", size)
		}
	}
}

func TestLoadOrCreateKeyRejectsLoosePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission check is skipped on windows")
	}
	seed := make([]byte, ed25519.SeedSize)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o601, 0o666} {
		dir := t.TempDir()
		path := filepath.Join(dir, keyFileName)
		if err := os.WriteFile(path, seed, mode); err != nil {
			t.Fatalf("write seed: %v", err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %04o: %v", mode, err)
		}
		_, err := LoadOrCreateKey(dir)
		if err == nil {
			t.Fatalf("mode %04o accepted, want refusal", mode)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %04o error %q does not mention chmod 600", mode, err)
		}
	}
}

func TestLoadOrCreateKeyAcceptsTightPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission modes are not enforced on windows")
	}
	seed := make([]byte, ed25519.SeedSize)
	for _, mode := range []os.FileMode{0o600, 0o400} {
		dir := t.TempDir()
		path := filepath.Join(dir, keyFileName)
		if err := os.WriteFile(path, seed, mode); err != nil {
			t.Fatalf("write seed: %v", err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %04o: %v", mode, err)
		}
		if _, err := LoadOrCreateKey(dir); err != nil {
			t.Fatalf("mode %04o refused: %v", mode, err)
		}
	}
}
