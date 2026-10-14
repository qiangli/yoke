package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// stubNpm writes an executable stub standing in for the provisioned npm: it
// appends its argv to log and optionally fails. Tests prove the install
// skip/run/marker logic without touching the network.
func stubNpm(t *testing.T, dir string, fail bool) (npm, log string) {
	t.Helper()
	log = filepath.Join(dir, "npm-argv.log")
	body := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if fail {
		body += "echo boom >&2\nexit 1\n"
	} else {
		body += "mkdir -p \"$3/node_modules\"\n"
	}
	npm = filepath.Join(dir, "npm")
	if err := os.WriteFile(npm, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return npm, log
}

func TestInstallNpmProdRunsThenSkips(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	npm, log := stubNpm(t, dir, false)
	pkgDir := filepath.Join(dir, "package")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "openclaw@2026.10.2 node=24.21.0 prod"

	if err := installNpmProd(ctx, npm, dir, pkgDir, want); err != nil {
		t.Fatalf("install: %v", err)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("stub npm never ran: %v", err)
	}
	for _, flag := range []string{"--omit=dev", "--legacy-peer-deps", "--prefix", pkgDir, "--no-audit"} {
		if !strings.Contains(string(argv), flag) {
			t.Fatalf("npm argv %q missing %q (production graph required)", argv, flag)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(pkgDir, npmProdMarker)); strings.TrimSpace(string(got)) != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
	// Second call: marker matches, npm must not run again (offline-safe).
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := installNpmProd(ctx, npm, dir, pkgDir, want); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("npm ran despite a valid marker — provisioned hosts must stay offline")
	}
}

func TestInstallNpmProdFailureWritesNoMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	npm, _ := stubNpm(t, dir, true)
	pkgDir := filepath.Join(dir, "package")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installNpmProd(ctx, npm, dir, pkgDir, "openclaw@2026.10.2 node=24.21.0 prod"); err == nil {
		t.Fatal("expected error from failing npm")
	}
	if _, err := os.Stat(filepath.Join(pkgDir, npmProdMarker)); !os.IsNotExist(err) {
		t.Fatal("failed install wrote a marker — a half-installed tree would be trusted")
	}
}

func TestInstallNpmProdStaleMarkerReinstalls(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	npm, log := stubNpm(t, dir, false)
	pkgDir := filepath.Join(dir, "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker names an older node: native modules may need a rebuild, so npm
	// must run again.
	if err := os.WriteFile(filepath.Join(pkgDir, npmProdMarker), []byte("openclaw@2026.10.2 node=22.23.2 prod"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installNpmProd(ctx, npm, dir, pkgDir, "openclaw@2026.10.2 node=24.21.0 prod"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatal("stale marker did not trigger reinstall")
	}
}

// seedBundled mimics the extracted tarball: bundled deps npm retires for
// replacement but never refills.
func seedBundled(t *testing.T, pkgDir string) {
	t.Helper()
	for name, ver := range map[string]string{
		"undici":              "8.11.2",
		"@openclaw/proxyline": "0.3.12",
	} {
		dir := filepath.Join(pkgDir, "node_modules", filepath.FromSlash(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":`+`"`+name+`","version":"`+ver+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("// pinned\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStashRestoreEmptied(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "package")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedBundled(t, pkgDir)

	stashed, bakRoot, err := stashBundledDeps(pkgDir)
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if len(stashed) != 2 {
		t.Fatalf("stashed %d deps, want 2 (plain + scoped)", len(stashed))
	}
	// Live tree is gone (moved aside); dot bookkeeping stays put.
	if _, err := os.Stat(filepath.Join(pkgDir, "node_modules", "undici")); !os.IsNotExist(err) {
		t.Fatal("stash left the live dir behind")
	}

	// npm refills undici at the same version, empties nothing else here, and
	// swaps proxyline for a different version: restore must keep npm's
	// undici and refuse the swapped proxyline.
	nm := filepath.Join(pkgDir, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "undici"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(nm, "undici", "package.json"), []byte(`{"version":"8.11.2"}`), 0o644)
	if err := os.MkdirAll(filepath.Join(nm, "@openclaw", "proxyline"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(nm, "@openclaw", "proxyline", "package.json"), []byte(`{"version":"9.9.9"}`), 0o644)
	if err := restoreEmptiedDeps(stashed); err == nil {
		t.Fatal("expected refusal when npm swaps a bundled dep version")
	}
	// Back out the swap: now undici stays npm-filled, proxyline restores.
	os.RemoveAll(filepath.Join(nm, "@openclaw", "proxyline"))
	if err := restoreEmptiedDeps(stashed); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(nm, "@openclaw", "proxyline", "index.js")); string(got) != "// pinned\n" {
		t.Fatalf("bundled proxyline not restored, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(nm, "undici", "index.js")); !os.IsNotExist(err) {
		t.Fatal("npm-filled undici must win over the stash, not be overwritten")
	}
	os.RemoveAll(bakRoot)
}

func TestInstallNpmProdRestoresGuttedBundled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Stub behaves like the observed npm: it retires the bundled dir for
	// replacement but never refills it.
	npm := filepath.Join(dir, "npm")
	stub := "#!/bin/sh\nrm -rf \"$3/node_modules/undici\"\nmkdir -p \"$3/node_modules/undici\"\n"
	if err := os.WriteFile(npm, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(dir, "package")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedBundled(t, pkgDir)

	want := "openclaw@2026.10.2 node=24.21.0 prod"
	if err := installNpmProd(ctx, npm, dir, pkgDir, want); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(pkgDir, "node_modules", "undici", "index.js")); string(got) != "// pinned\n" {
		t.Fatalf("bundled undici lost to npm retire-never-refill, got %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(pkgDir, npmProdMarker)); strings.TrimSpace(string(got)) != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

func TestNpmPackageDir(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "openclaw", "2026.10.2", "package")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := npmPackageDir(filepath.Join(pkg, "openclaw.mjs"))
	if err != nil {
		t.Fatalf("npmPackageDir: %v", err)
	}
	if got != pkg {
		t.Fatalf("npmPackageDir = %q, want %q", got, pkg)
	}
	if _, err := npmPackageDir(filepath.Join(dir, "nowhere", "x.mjs")); err == nil {
		t.Fatal("expected error with no package.json above")
	}
}

func TestNodeVersionName(t *testing.T) {
	if got := nodeVersionName(""); got != DefaultVersion {
		t.Fatalf("empty version = %q, want default %q", got, DefaultVersion)
	}
	if got := nodeVersionName("v24.21.0"); got != "24.21.0" {
		t.Fatalf("v-prefix not trimmed: %q", got)
	}
	tool := binmgr.Tool{Name: "openclaw", Version: "2026.10.2"}
	if want := "openclaw@2026.10.2 node=24.21.0 prod"; npmMarkerWant(tool, "24.21.0") != want {
		t.Fatalf("marker = %q, want %q", npmMarkerWant(tool, "24.21.0"), want)
	}
}
