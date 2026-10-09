package agentlaunch

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// A managed baseline tool resolves to its pinned cache path — not to a PATH
// name — carrying the recipe's env and the binmgr declaration, with no
// network touched at resolve time.
func TestResolveManagedToolPointsAtThePin(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv(UnsafeLaunchEnv, "1")
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	t.Setenv(fleet.ToolBinaryOverrideEnv("opencode"), "")

	l, err := ResolveWithCatalog("opencode:deepseek-v4-pro", Options{DryRun: true}, testCatalog(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if l.ManagedTool == nil {
		t.Fatalf("opencode resolved unmanaged: %+v", l)
	}
	want := filepath.Join(cache, "opencode", l.ManagedTool.Version, binmgr.BinaryName("opencode"))
	if l.Tool != want {
		t.Fatalf("Tool = %q, want the cache slot %q", l.Tool, want)
	}
	if l.ToolName != "opencode" {
		t.Fatalf("ToolName = %q", l.ToolName)
	}
	if !contains(l.Env, "OPENCODE_DISABLE_AUTOUPDATE=1") {
		t.Fatalf("Env = %v, want the self-update switch", l.Env)
	}
	if !l.FailEvents.Declared() {
		t.Fatal("FailEvents not carried from the recipe")
	}
	if _, err := os.Stat(filepath.Join(cache, "opencode")); !os.IsNotExist(err) {
		t.Fatal("resolve touched the cache; install belongs to EnsureManaged")
	}
}

// The ACP launch keeps the pin too: it renders argv from acp_exec but must not
// put the bare binary name back into argv[0].
func TestResolveManagedToolKeepsPinOverACP(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv(UnsafeLaunchEnv, "1")
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	t.Setenv(fleet.ToolBinaryOverrideEnv("opencode"), "")
	l, err := ResolveWithCatalog("opencode", Options{ACP: true, DryRun: true}, testCatalog(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Tool, cache) || l.ManagedTool == nil {
		t.Fatalf("ACP launch lost the pin: %+v", l)
	}
	if strings.Join(l.Args, " ") != "acp" {
		t.Fatalf("args = %q", l.Args)
	}
}

// The override is the operator's explicit word: the per-tool variable wins,
// the launch carries no managed install, and nothing is downloaded.
func TestResolveManagedToolHonoursExplicitOverride(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv(UnsafeLaunchEnv, "1")
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv(fleet.ToolBinaryOverrideEnv("opencode"), "/opt/homebrew/bin/opencode")
	l, err := ResolveWithCatalog("opencode:deepseek-v4-pro", Options{DryRun: true}, testCatalog(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if l.Tool != "/opt/homebrew/bin/opencode" || l.ManagedTool != nil {
		t.Fatalf("override ignored: %+v", l)
	}
	if p, err := EnsureManaged(context.Background(), l); err != nil || p != "/opt/homebrew/bin/opencode" {
		t.Fatalf("EnsureManaged on an override = %q, %v", p, err)
	}
}

func zipWith(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func managedLaunch(url, sha string) Launch {
	return Launch{
		ToolName: "pinned",
		ManagedTool: &binmgr.Tool{Name: "pinned", Version: "9.9.9", Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: url, SHA256: sha, Binary: "pinned"},
		}},
	}
}

// First use installs the pin into the bashy cache from the vendor URL and
// verifies the digest; the second use is a cache hit with no request.
func TestEnsureManagedInstallsPinOnFirstUse(t *testing.T) {
	payload := []byte("#!/bin/sh\necho pinned\n")
	archive := zipWith(t, "pinned", payload)
	sum := sha256.Sum256(archive)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)

	l := managedLaunch(srv.URL+"/pinned.zip", hex.EncodeToString(sum[:]))
	l.Tool, _ = binmgr.InstallPath(*l.ManagedTool)
	path, err := EnsureManaged(context.Background(), l)
	if err != nil {
		t.Fatalf("EnsureManaged: %v", err)
	}
	if path != l.Tool || !strings.HasPrefix(path, cache) {
		t.Fatalf("installed at %q, launch names %q", path, l.Tool)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, payload) {
		t.Fatalf("cached bytes differ")
	}
	if _, err := EnsureManaged(context.Background(), l); err != nil || hits != 1 {
		t.Fatalf("second use: err=%v hits=%d (want a cache hit)", err, hits)
	}
}

// A digest mismatch is the launch's failure: nothing is cached, nothing falls
// back to a PATH binary of the same name, and the error names the override.
func TestEnsureManagedWrongDigestRefusesWithoutFallback(t *testing.T) {
	archive := zipWith(t, "pinned", []byte("tampered"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	// A same-named binary on PATH that must NOT be picked up.
	bin := t.TempDir()
	decoy := filepath.Join(bin, binmgr.BinaryName("pinned"))
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	l := managedLaunch(srv.URL+"/pinned.zip", strings.Repeat("0", 64))
	l.Tool, _ = binmgr.InstallPath(*l.ManagedTool)
	path, err := EnsureManaged(context.Background(), l)
	if err == nil {
		t.Fatalf("tampered artifact installed at %q", path)
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") || !strings.Contains(err.Error(), fleet.ToolBinaryOverrideEnv("pinned")) {
		t.Fatalf("error = %v", err)
	}
	if path != "" || path == decoy {
		t.Fatalf("fell back to %q", path)
	}
	if _, serr := os.Stat(l.Tool); !os.IsNotExist(serr) {
		t.Fatal("a rejected download left a binary in the cache")
	}
}

// The recipe's pairs replace inherited values: the operator's shell cannot
// re-enable a pinned tool's self-update.
func TestApplyLaunchEnvReplacesInherited(t *testing.T) {
	env := []string{"HOME=/h", "OPENCODE_DISABLE_AUTOUPDATE=0", "PATH=/p"}
	got := ApplyLaunchEnv(env, Launch{Env: []string{"OPENCODE_DISABLE_AUTOUPDATE=1", "NEW=1"}})
	want := []string{"HOME=/h", "PATH=/p", "OPENCODE_DISABLE_AUTOUPDATE=1", "NEW=1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env = %v, want %v", got, want)
	}
	if out := ApplyLaunchEnv(env, Launch{}); &out[0] != &env[0] {
		t.Fatal("no-op launch should return env unchanged")
	}
	if len(ApplyLaunchEnv(env, Launch{Env: []string{"BROKEN"}})) != len(env) {
		t.Fatal("a malformed pair was applied")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
