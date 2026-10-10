package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const storePathTestUUID = "33333333-3333-4333-8333-333333333333"

// A store whose path is a regular file — or sits beneath one — is BROKEN, not
// empty. Windows reports lookups beneath a file as not-found, so this is the
// case that used to read as a fresh host there and let callers fall back to
// legacy names without validation.
func TestInstanceStoreBrokenPathFailsClosed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "instances")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"store is a file":        file,
		"store beneath a file":   filepath.Join(file, "instances"),
		"store two below a file": filepath.Join(file, "a", "instances"),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewInstanceStore(dir)
			if all, err := s.List(); err == nil {
				t.Fatalf("List on a broken store path returned %v, nil", all)
			}
			_, err := s.Get(storePathTestUUID)
			if err == nil || errors.Is(err, ErrInstanceUnknown) {
				t.Fatalf("Get on a broken store path must be a store error, not unknown: %v", err)
			}
			if err := checkMissingDir(dir); err == nil {
				t.Fatal("checkMissingDir accepted a broken path")
			}
		})
	}
}

// A store directory that does not exist yet (fresh host) is empty, and an id
// in it is unknown — the compatibility half the check above must not break.
func TestInstanceStoreMissingDirIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet", "instances")
	s := NewInstanceStore(dir)
	if all, err := s.List(); err != nil || len(all) != 0 {
		t.Fatalf("missing store: List = %v, %v; want empty, nil", all, err)
	}
	if _, err := s.Get(storePathTestUUID); !errors.Is(err, ErrInstanceUnknown) {
		t.Fatalf("missing store: Get err = %v; want ErrInstanceUnknown", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(storePathTestUUID); !errors.Is(err, ErrInstanceUnknown) {
		t.Fatalf("empty store: Get err = %v; want ErrInstanceUnknown", err)
	}
	if err := checkMissingDir(dir); err != nil {
		t.Fatalf("an existing store directory is sound: %v", err)
	}
}

// A mailbox file that is missing in a sound directory is no mail; one whose
// directory is a regular file is an error.
func TestReadLinesBrokenParentFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if lines, err := readLines(filepath.Join(dir, "absent")); err != nil || lines != nil {
		t.Fatalf("missing file: %v, %v", lines, err)
	}
	file := filepath.Join(dir, "mail")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLines(filepath.Join(file, "inbox")); err == nil {
		t.Fatal("a mailbox beneath a regular file read as empty")
	}
}
