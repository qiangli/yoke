package browsercmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
