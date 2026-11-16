package browsercmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/browser/live"
)

func TestSetupLiveWalksTheHumanThrough(t *testing.T) {
	var opened, revealed, copied string
	saved := hooks
	t.Cleanup(func() { hooks = saved })
	hooks = setupHooks{
		openExtensions: func() error { opened = "chrome://extensions"; return nil },
		reveal:         func(dir string) error { revealed = dir; return nil },
		copyText:       func(s string) error { copied = s; return nil },
	}
	dir := filepath.Join(t.TempDir(), "ext")
	out, errOut, code := runTool(t, "setup", "live", dir)
	if code != 0 {
		t.Fatalf("setup live exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "page.js")); err != nil {
		t.Fatalf("extension not extracted: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if opened == "" || revealed != abs || copied != abs {
		t.Fatalf("desktop steps not taken: opened=%q revealed=%q copied=%q", opened, revealed, copied)
	}
	for _, want := range []string{"1. chrome://extensions", "2. drag the folder", "3. on the tab", "opened chrome://extensions, opened the folder, copied its path"} {
		if !strings.Contains(out, want) {
			t.Fatalf("setup output lacks %q:\n%s", want, out)
		}
	}
}

func TestSetupLiveNoOpenAndBestEffort(t *testing.T) {
	calls := 0
	saved := hooks
	t.Cleanup(func() { hooks = saved })
	fail := func() error { calls++; return errors.New("no desktop") }
	hooks = setupHooks{
		openExtensions: fail,
		reveal:         func(string) error { return fail() },
		copyText:       func(string) error { return fail() },
	}
	if _, errOut, code := runTool(t, "--no-open", "setup", "live", filepath.Join(t.TempDir(), "a")); code != 0 || calls != 0 {
		t.Fatalf("--no-open: exit %d, %d desktop calls (%s)", code, calls, errOut)
	}
	// Desktop failures never fail setup; the steps still print.
	out, _, code := runTool(t, "setup", "live", filepath.Join(t.TempDir(), "b"))
	if code != 0 || calls != 3 || !strings.Contains(out, "1. chrome://extensions") || strings.Contains(out, "(opened") {
		t.Fatalf("best-effort desktop steps: exit %d calls %d\n%s", code, calls, out)
	}
}

// With a store listing, setup sends the human to the store (one click, the
// store keeps it current); --unpacked keeps the developer path.
func TestSetupLiveUsesTheStoreWhenListed(t *testing.T) {
	savedHooks, savedURL := hooks, live.ChromeWebStoreURL
	t.Cleanup(func() { hooks, live.ChromeWebStoreURL = savedHooks, savedURL })
	live.ChromeWebStoreURL = "https://chromewebstore.google.com/detail/example"
	var opened string
	hooks = setupHooks{
		openExtensions: func() error { t.Fatal("store path opened chrome://extensions"); return nil },
		openURL:        func(u string) error { opened = u; return nil },
		reveal:         func(string) error { return nil },
		copyText:       func(string) error { return nil },
	}
	dir := filepath.Join(t.TempDir(), "ext")
	out, errOut, code := runTool(t, "setup", "live", dir)
	if code != 0 || opened != live.ChromeWebStoreURL || !strings.Contains(out, "1. Add to Chrome") {
		t.Fatalf("store path: exit %d opened %q\n%s%s", code, opened, out, errOut)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("store path must not extract the unpacked copy")
	}
	hooks.openExtensions = func() error { return nil }
	out, _, code = runTool(t, "--unpacked", "setup", "live", dir)
	if code != 0 || !strings.Contains(out, "1. chrome://extensions") {
		t.Fatalf("--unpacked: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("--unpacked did not extract: %v", err)
	}
}
