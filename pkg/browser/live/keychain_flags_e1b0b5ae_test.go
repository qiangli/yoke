package live

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func liveE2EAllocatorOptions(extDir, profileDir, exe string) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", "new"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.WindowSize(1200, 900),
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("disable-extensions-except", extDir),
		chromedp.Flag("load-extension", extDir),
		chromedp.Flag("disable-features", "DisableLoadExtensionCommandLineSwitch"),
	}
	if exe != "" {
		opts = append(opts, chromedp.ExecPath(exe))
	}
	return opts
}

// Story e1b0b5ae: assert live extension E2E allocator flags via a fake shell
// binary without launching a real browser.
func TestLiveE2EAllocatorOptionsIsolateKeychain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fake browser recorder")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakeExe := filepath.Join(dir, "fake-chrome.sh")
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argsFile+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, "profile")
	ext := filepath.Join(dir, "ext")
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), liveE2EAllocatorOptions(ext, profile, fakeExe)...)
	defer cancelAlloc()
	ctx, cancelCtx := chromedp.NewContext(alloc)
	defer cancelCtx()
	_ = chromedp.Run(ctx)

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake chrome did not record argv: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{
		"--use-mock-keychain",
		"--password-store=basic",
		"--headless=new",
		"--user-data-dir=" + profile,
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("live E2E argv missing %q: %v", want, args)
		}
	}
}
