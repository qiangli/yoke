package solo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

func writeFakeChromeArgRecorder(t *testing.T) (exePath, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fake browser recorder")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "recorded-args.txt")
	exePath = filepath.Join(dir, "fake-chrome.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\nexit 1\n"
	if err := os.WriteFile(exePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exePath, argsFile
}

// Story e1b0b5ae: verify rendered solo Chrome argv without launching Chrome.
// Ephemeral launches pass --use-mock-keychain, --password-store=basic, and a
// throwaway --user-data-dir; already-initialized persistent profiles omit the
// keychain overrides; real OS browser profiles are refused before exec.
func TestSoloEnsureReadyKeychainAndProfileArgvWithoutRealChrome(t *testing.T) {
	exe, argsFile := writeFakeChromeArgRecorder(t)

	// 1. Default ephemeral solo launch (no UserDataDir).
	c := New(Config{ChromePath: exe})
	_ = c.EnsureReady(context.Background())
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake chrome did not record argv: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{"--use-mock-keychain", "--password-store=basic", "--headless=new"} {
		if !slices.Contains(args, want) {
			t.Fatalf("ephemeral solo argv missing %q: %v", want, args)
		}
	}
	hasTempDir := false
	for _, a := range args {
		if strings.HasPrefix(a, "--user-data-dir=") && strings.Contains(a, "bashy-browser-solo-") {
			hasTempDir = true
		}
	}
	if !hasTempDir {
		t.Fatalf("ephemeral solo argv missing throwaway bashy-browser-solo-* user-data-dir: %v", args)
	}

	// 2. Initialized persistent profile (contains Local State) omits mock-keychain/password-store.
	_ = os.Remove(argsFile)
	persist := filepath.Join(t.TempDir(), "persistent-profile")
	if err := os.MkdirAll(persist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persist, "Local State"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := New(Config{ChromePath: exe, UserDataDir: persist})
	_ = cp.EnsureReady(context.Background())
	rawPersist, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake chrome did not record persistent argv: %v", err)
	}
	persistArgs := strings.Split(strings.TrimSpace(string(rawPersist)), "\n")
	for _, unwanted := range []string{"--use-mock-keychain", "--password-store=basic"} {
		if slices.Contains(persistArgs, unwanted) {
			t.Fatalf("initialized persistent solo profile must not pass %q: %v", unwanted, persistArgs)
		}
	}
	if !slices.Contains(persistArgs, "--user-data-dir="+persist) {
		t.Fatalf("persistent solo argv missing explicit user-data-dir %q: %v", persist, persistArgs)
	}

	// 3. Real OS browser profile is refused before launching anything.
	_ = os.Remove(argsFile)
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	var realDir string
	switch runtime.GOOS {
	case "darwin":
		realDir = filepath.Join(fakeHome, "Library", "Application Support", "Google", "Chrome")
	default:
		realDir = filepath.Join(fakeHome, ".config", "google-chrome")
	}
	cr := New(Config{ChromePath: exe, UserDataDir: realDir})
	if err := cr.EnsureReady(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing to use real browser profile") {
		t.Fatalf("real profile err = %v, want refusal", err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("refused real profile must not execute browser binary")
	}
}

func TestResolveChromePathHonorsMissingExplicitPath(t *testing.T) {
	c := New(Config{ChromePath: "/definitely/not/a/chrome"})
	if got := c.ResolveChromePath(); got != "" {
		t.Fatalf("ResolveChromePath=%q, want empty", got)
	}
}

func TestLiveSoloSkippedWhenNoChrome(t *testing.T) {
	// Opt-in only. This test LAUNCHES a real Chrome, and on a machine with
	// Chrome installed but no GUI/WindowServer session (headless CI, a weave
	// fleet worker, an ssh session) that launch aborts in _RegisterApplication
	// (SIGABRT) BEFORE the launchability check below can skip — firing a
	// crash-reporter popup on every `go test ./...`. Gate it so an automated
	// run never touches Chrome; set BASHY_BROWSER_LIVE=1 to run it by hand on a
	// desktop where a live smoke check of the solo driver is actually useful.
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: "1+1"})
	if err != nil {
		t.Skipf("Chrome found but not launchable in this environment: %v", err)
	}
	if !res.Success || res.Data != "2" {
		t.Fatalf("unexpected live result: %#v", res)
	}
}

// TestLiveSoloNavigateBodyTextInCSS pins todo f1cd947e: navigate read the
// page with a plain-text DOM search for "body", which also matched the
// CSS rule `body{...}`; that text node has no box model, so the
// visibility wait never finished and every real page timed out.
func TestLiveSoloNavigateBodyTextInCSS(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>t</title><style>body{margin:auto}</style><body><p>hello body</p></body>`))
	}))
	defer srv.Close()
	c := New(Config{})
	if !c.Available(context.Background()) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionNavigate, URL: srv.URL})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if !res.Success || !strings.Contains(res.Content, "hello body") {
		t.Fatalf("navigate result after %s: %#v", time.Since(start), res)
	}
}
