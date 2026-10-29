package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const DefaultURL = "http://localhost:9222"

func LaunchChrome(chromePath string, port int, userDataDir string) (pid int, resolved string, err error) {
	if port <= 0 {
		port = 9222
	}
	if portInUse(port) {
		return 0, "", fmt.Errorf("port %d is already in use", port)
	}
	resolved = chromePath
	if resolved == "" {
		resolved, err = DefaultResolver().Resolve(context.Background(), "")
		if err != nil {
			return 0, "", err
		}
	}
	if resolved == "" {
		return 0, "", fmt.Errorf("Chromium-family browser not found")
	}
	if _, err := os.Stat(resolved); err != nil {
		return 0, "", fmt.Errorf("Chrome at %q: %w", resolved, err)
	}
	if userDataDir == "" {
		userDataDir = DefaultUserDataDir(port)
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return 0, "", fmt.Errorf("user-data-dir: %w", err)
	}
	cmd := exec.Command(resolved,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		"--headless=new",
		"--no-first-run",
		"--no-default-browser-check",
	)
	cmd.SysProcAttr = browserDetachedSysProcAttr()
	if err := startBrowserDetached(cmd); err != nil {
		return 0, "", fmt.Errorf("launch chrome: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if portInUse(port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, resolved, nil
}

// Resolver selects an installed browser first and provisions the pinned
// Chrome Headless Shell only when the host has none. Its functions are fields
// so unit tests can prove the decision without touching the network.
type Resolver struct {
	Detect func() string
	Cached func(binmgr.Tool) (string, bool)
	Ensure func(context.Context, binmgr.Tool) (string, error)
}

func DefaultResolver() Resolver {
	return Resolver{Detect: DetectChrome, Cached: binmgr.Cached, Ensure: binmgr.Ensure}
}

// CachedPath resolves only explicit, installed, or already-provisioned
// browsers. It never downloads and is suitable for status checks.
func (r Resolver) CachedPath(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	if r.Detect != nil {
		if path := r.Detect(); path != "" {
			return path
		}
	}
	if r.Cached != nil {
		if path, ok := r.Cached(binmgr.ChromeHeadlessShellTool()); ok {
			return path
		}
	}
	return ""
}

// Resolve is the zero-setup path used by browser actions: explicit path,
// installed browser, cached managed shell, then checksum-verified provision.
func (r Resolver) Resolve(ctx context.Context, explicit string) (string, error) {
	if path := r.CachedPath(explicit); path != "" {
		return path, nil
	}
	if explicit != "" {
		return "", fmt.Errorf("browser at %q does not exist", explicit)
	}
	if r.Ensure == nil {
		return "", fmt.Errorf("no browser provisioner configured")
	}
	path, err := r.Ensure(ctx, binmgr.ChromeHeadlessShellTool())
	if err != nil {
		return "", fmt.Errorf("provision Chrome Headless Shell: %w", err)
	}
	return path, nil
}

func portInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

func DetectChrome() string {
	if p := os.Getenv("CHROME"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, p := range platformChromePaths(runtime.GOOS) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range platformChromeNames(runtime.GOOS) {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func platformChromePaths(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "linux":
		return []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
		}
	case "windows":
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}
	}
	return nil
}

func platformChromeNames(goos string) []string {
	names := []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "chrome"}
	if goos == "linux" {
		return append([]string{"microsoft-edge", "microsoft-edge-stable"}, names...)
	}
	if goos == "windows" {
		return append([]string{"msedge", "msedge.exe", "chrome.exe"}, names...)
	}
	return names
}

// DefaultUserDataDir is stable across one-shot CLI invocations, which is what
// lets probe mode retain one page until `browser stop` is called.
func DefaultUserDataDir(port int) string {
	if port <= 0 {
		port = 9222
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "bashy", fmt.Sprintf("browser-probe-%d", port))
}
