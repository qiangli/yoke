package govulncheck

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestDefaultVersion(t *testing.T) {
	if DefaultVersion != "v1.9.0" {
		t.Fatalf("DefaultVersion = %q, want v1.9.0", DefaultVersion)
	}
}

func TestCachedBinary_HitAvoidsRebuild(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cacheRoot)
	t.Setenv("BASHY_OFFLINE", "1") // Offline ensures no build is attempted

	version := "v1.9.0"
	targetDir := filepath.Join(cacheRoot, "govulncheck", version)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	binName := binmgr.BinaryName("govulncheck")
	mockBin := filepath.Join(targetDir, binName)
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(mockBin, []byte("@echo off\r\necho govulncheck mock\r\n"), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(mockBin, []byte("#!/bin/sh\necho govulncheck mock\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	cached := CachedBinary(version)
	if cached != mockBin {
		t.Fatalf("CachedBinary = %q, want %q", cached, mockBin)
	}

	got, err := Ensure(context.Background(), version)
	if err != nil {
		t.Fatalf("Ensure returned error on cache hit: %v", err)
	}
	if got != mockBin {
		t.Fatalf("Ensure = %q, want %q", got, mockBin)
	}
}

func TestOffline_NotCachedFails(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "1")

	_, err := Ensure(context.Background(), "v1.9.0")
	if err == nil {
		t.Fatal("Ensure expected error when offline and not cached, got nil")
	}
	if !strings.Contains(err.Error(), "BASHY_OFFLINE=1 forbids building it") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCacheDir_RespectsBashyHome(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", "")
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)

	dir, err := CacheDir()
	if err != nil {
		t.Fatalf("CacheDir error: %v", err)
	}
	want := filepath.Join(home, "cache", "bin")
	if dir != want {
		t.Errorf("CacheDir = %q, want %q", dir, want)
	}
}
