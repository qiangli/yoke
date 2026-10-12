package feed

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

const keyFileName = "ed25519.key"

// KeyDir returns the feed key directory: $BASHY_HOME/feed when BASHY_HOME is
// set to a non-empty value, otherwise ~/.bashy/feed. If the home directory
// cannot be determined, the relative path .bashy/feed is returned.
func KeyDir() string {
	if home := os.Getenv("BASHY_HOME"); home != "" {
		return filepath.Join(home, "feed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".bashy", "feed")
	}
	return filepath.Join(home, ".bashy", "feed")
}

// LoadOrCreateKey returns the ed25519 signing key stored in dir/ed25519.key,
// generating and persisting a new key on first use. The key file format is the
// raw 32-byte ed25519 seed in binary form (no PEM, no hex); the private key is
// re-derived from the seed on every load. The directory is created 0700 and
// the seed file is written 0600 atomically via temp file plus rename. On unix
// an existing key file with group or other permissions is refused; fix it
// with chmod 600. The permission check is skipped on windows.
func LoadOrCreateKey(dir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dir, keyFileName)
	seed, err := os.ReadFile(path)
	if err == nil {
		if err := checkKeyPerms(path); err != nil {
			return nil, err
		}
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("feed: %s: malformed key file: want %d-byte raw seed, got %d bytes", path, ed25519.SeedSize, len(seed))
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("feed: read %s: %w", path, err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("feed: generate ed25519 key: %w", err)
	}
	if err := writeSeedFile(dir, path, priv.Seed()); err != nil {
		return nil, err
	}
	return priv, nil
}

func checkKeyPerms(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("feed: stat %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("feed: %s: key file must not be readable or writable by group or others (mode %04o); chmod 600 %s", path, info.Mode().Perm(), path)
	}
	return nil
}

func writeSeedFile(dir, path string, seed []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("feed: create key directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ed25519.key.tmp-*")
	if err != nil {
		return fmt.Errorf("feed: create temp key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("feed: chmod temp key file: %w", err)
	}
	if _, err := tmp.Write(seed); err != nil {
		tmp.Close()
		return fmt.Errorf("feed: write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("feed: sync key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("feed: close key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("feed: install key file %s: %w", path, err)
	}
	return nil
}
