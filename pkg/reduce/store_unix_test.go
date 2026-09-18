// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build !windows

package reduce

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpillIs0600 pins the contract's 0600 obligation: an elided region can
// contain a credential and the handle makes it retrievable later, so the spill
// inherits the same file-mode gate as any other capture path (§2.7). File modes
// are not enforced on Windows, hence the build tag.
func TestSpillIs0600(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	digest, err := s.Put([]byte("sensitive complete output"))
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(digest, "sha256:")
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("spill blob mode = %o, want 0600", perm)
	}
}

func TestExistingSpillPermissionsAreFixed(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	content := []byte("permissions")
	digest, err := s.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(digest, "sha256:")
	if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(content); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("existing spill mode = %o, want 0600", info.Mode().Perm())
	}
}
