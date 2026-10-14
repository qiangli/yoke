package grype

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Name != "grype" || s.Repo != "anchore/grype" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "grype" || s.AssetMatch == nil {
		t.Fatalf("spec member/assetMatch incorrect: %+v", s)
	}
	if Spec("v0.119.0").Version != "v0.119.0" {
		t.Fatal("version override not honored")
	}
}

func TestAssetMatch(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		// 5 supported targets
		{"grype_0.120.1_darwin_amd64.tar.gz", "darwin", "amd64", true},
		{"grype_0.120.1_darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"grype_0.120.1_linux_amd64.tar.gz", "linux", "amd64", true},
		{"grype_0.120.1_linux_arm64.tar.gz", "linux", "arm64", true},
		{"grype_0.120.1_windows_amd64.zip", "windows", "amd64", true},

		// negative cases
		{"grype_0.120.1_darwin_amd64.tar.gz", "linux", "amd64", false},         // wrong os
		{"grype_0.120.1_darwin_arm64.tar.gz", "darwin", "amd64", false},        // wrong arch
		{"grype_0.120.1_linux_amd64.deb", "linux", "amd64", false},             // deb package
		{"grype_0.120.1_linux_amd64.rpm", "linux", "amd64", false},             // rpm package
		{"grype_0.120.1_windows_amd64.zip", "darwin", "amd64", false},          // wrong os
		{"grype_0.120.1_checksums.txt", "linux", "amd64", false},               // checksums
		{"grype_0.120.1_checksums.txt.sigstore.json", "linux", "amd64", false}, // sigstore bundle
		{"grype_0.120.1_linux_ppc64le.tar.gz", "linux", "amd64", false},        // wrong arch
	}

	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %q, %q) = %v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestPins(t *testing.T) {
	targets := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"}
	for _, target := range targets {
		sha, ok := binmgr.PinnedSHA256("grype", DefaultVersion, target)
		if !ok || len(sha) != 64 {
			t.Errorf("grype pin missing or invalid for %s: ok=%v, sha=%q", target, ok, sha)
		}
	}
}

func TestOfflineSkip_ReturnsSkippedStatusNotSuccess(t *testing.T) {
	t.Setenv("BASHY_OFFLINE", "1")
	t.Setenv("GRYPE_DB_CACHE_DIR", filepath.Join(t.TempDir(), "empty-grype-db"))

	status, err := EnsureDB(context.Background(), "")
	if status != StatusSkippedOffline {
		t.Fatalf("EnsureDB offline status = %q, want %q", status, StatusSkippedOffline)
	}
	if err == nil {
		t.Fatal("EnsureDB returned nil error on offline skip; must never be reported as a pass")
	}
	if !errors.Is(err, ErrOfflineNoDB) {
		t.Fatalf("EnsureDB error = %v, want ErrOfflineNoDB", err)
	}

	// Also verify Run returns the skipped status
	runStatus, runErr := Run(context.Background(), "version")
	if runStatus != StatusSkippedOffline {
		t.Fatalf("Run offline status = %q, want %q", runStatus, StatusSkippedOffline)
	}
	if runErr == nil {
		t.Fatal("Run returned nil error on offline skip; must never be reported as a pass")
	}
}

func TestEnsureDB_DownloadFailure_ReturnsSkippedStatus(t *testing.T) {
	t.Setenv("BASHY_OFFLINE", "0")
	t.Setenv("GRYPE_DB_CACHE_DIR", filepath.Join(t.TempDir(), "failing-db"))

	// Create a mock executable that fails
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fake-grype")
	if runtime.GOOS == "windows" {
		fakeBin += ".bat"
		if err := os.WriteFile(fakeBin, []byte("@echo off\r\nexit 1\r\n"), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	status, err := EnsureDB(context.Background(), fakeBin)
	if status != StatusSkippedOffline {
		t.Fatalf("EnsureDB download failure status = %q, want %q", status, StatusSkippedOffline)
	}
	if err == nil {
		t.Fatal("EnsureDB returned nil error on download failure; must never be reported as a pass")
	}
	if !errors.Is(err, ErrOfflineNoDB) {
		t.Fatalf("EnsureDB error = %v, want ErrOfflineNoDB", err)
	}
}

func TestCachedDB_ReusedAndAutoUpdateDisabled(t *testing.T) {
	dbDir := filepath.Join(t.TempDir(), "cached-grype-db")
	schemaDir := filepath.Join(dbDir, "6")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(schemaDir, "vulnerability.db")
	if err := os.WriteFile(dbFile, []byte("sqlite dummy database"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GRYPE_DB_CACHE_DIR", dbDir)
	t.Setenv("BASHY_OFFLINE", "1") // Even offline, a cached DB succeeds

	gotDir, err := EnsureDB(context.Background(), "")
	if err != nil {
		t.Fatalf("EnsureDB with cached DB failed: %v", err)
	}
	if gotDir != dbDir {
		t.Fatalf("EnsureDB = %q, want %q", gotDir, dbDir)
	}

	env := DBEnv(dbDir)
	var hasCacheDir, hasAutoUpdateFalse bool
	for _, e := range env {
		if e == "GRYPE_DB_CACHE_DIR="+dbDir {
			hasCacheDir = true
		}
		if e == "GRYPE_DB_AUTO_UPDATE=false" {
			hasAutoUpdateFalse = true
		}
	}
	if !hasCacheDir {
		t.Errorf("DBEnv missing GRYPE_DB_CACHE_DIR=%s: %v", dbDir, env)
	}
	if !hasAutoUpdateFalse {
		t.Errorf("DBEnv missing GRYPE_DB_AUTO_UPDATE=false when DB is cached: %v", env)
	}
}

func TestDBCacheDir_RespectsBashyHomeAndBinmgrCache(t *testing.T) {
	t.Setenv("GRYPE_DB_CACHE_DIR", "")

	// 1. Respects BASHY_HOME
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dir, err := DBCacheDir()
	if err != nil {
		t.Fatalf("DBCacheDir error: %v", err)
	}
	wantHome := filepath.Join(home, "cache", "grype", "db")
	if dir != wantHome {
		t.Errorf("DBCacheDir with BASHY_HOME = %q, want %q", dir, wantHome)
	}

	// 2. Respects BASHY_BIN_CACHE when BASHY_HOME is unset
	t.Setenv("BASHY_HOME", "")
	binCache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", binCache)
	dir2, err := DBCacheDir()
	if err != nil {
		t.Fatalf("DBCacheDir error: %v", err)
	}
	wantBin := filepath.Join(binCache, "grype", "db")
	if dir2 != wantBin {
		t.Errorf("DBCacheDir with BASHY_BIN_CACHE = %q, want %q", dir2, wantBin)
	}
}
