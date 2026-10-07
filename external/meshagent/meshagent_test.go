package meshagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit check is unix-shaped")
	}
	// A non-executable $OUTPOST_BIN resolves false.
	t.Setenv("OUTPOST_BIN", filepath.Join(t.TempDir(), "nope"))
	if _, ok := Resolve(); ok {
		t.Error("missing $OUTPOST_BIN should resolve false")
	}
	// A real executable resolves.
	bin := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BIN", bin)
	got, ok := Resolve()
	if !ok || got != bin {
		t.Errorf("Resolve() = %q,%v; want %q,true", got, ok, bin)
	}
	if !Installed() {
		t.Error("Installed() = false, want true")
	}
}

func TestResolveSiblingWins(t *testing.T) {
	dir, stale := t.TempDir(), t.TempDir()
	name := "outpost"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(dir, name)
	for _, p := range []string{sibling, filepath.Join(stale, name)} {
		if err := os.WriteFile(p, []byte("not executed"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OUTPOST_BIN", filepath.Join(stale, name))
	t.Setenv("PATH", stale)
	got, ok := resolve(filepath.Join(dir, "bashy"))
	if !ok || got != sibling {
		t.Fatalf("resolve = %q, %v; want %q", got, ok, sibling)
	}
	// Removing the paired file restores the explicit override before PATH.
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	got, ok = resolve(filepath.Join(dir, "bashy"))
	if !ok || got != filepath.Join(stale, name) {
		t.Fatalf("override = %q, %v", got, ok)
	}
}

func TestSpecUsesHostReleaseAndOutpostAsset(t *testing.T) {
	old := HostVersion
	t.Cleanup(func() { HostVersion = old })
	HostVersion = func() string { return "1.2.3-dev" }
	t.Setenv("OUTPOST_VERSION", "")
	spec := Spec("")
	if spec.Repo != "qiangli/bashy" || spec.Version != "v1.2.3-dev" {
		t.Fatalf("spec = %+v", spec)
	}
	for _, tc := range []struct {
		name, os, arch string
		want           bool
	}{
		{"outpost-1.2.3-dev-linux-amd64", "linux", "amd64", true},
		{"outpost-1.2.3-dev-windows-amd64.exe", "windows", "amd64", true},
		{"outpost-1.2.3-dev-linux-amd64.sha256", "linux", "amd64", false},
		{"bashy-scratch-linux-amd64", "linux", "amd64", false},
		{"outpost-1.2.3-dev-linux-arm64", "linux", "amd64", false},
	} {
		if got := spec.AssetMatch(tc.name, tc.os, tc.arch); got != tc.want {
			t.Errorf("match %s = %v", tc.name, got)
		}
	}
	t.Setenv("OUTPOST_VERSION", "v2.0.0")
	if got := Spec("").Version; got != "v2.0.0" {
		t.Fatal(got)
	}
	if got := Spec("v3.0.0").Version; got != "v3.0.0" {
		t.Fatal(got)
	}
	t.Setenv("OUTPOST_VERSION", "")
	HostVersion = func() string { return "" }
	if got := Spec("").Version; got != "latest" {
		t.Fatal(got)
	}
}
