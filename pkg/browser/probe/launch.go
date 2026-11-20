package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const DefaultURL = "http://localhost:9222"

func LaunchChrome(chromePath string, port int, userDataDir string) (pid int, resolved string, err error) {
	process, err := LaunchBrowser(chromePath, port, userDataDir)
	if err != nil {
		return 0, "", err
	}
	return process.PID, process.Path, nil
}

// BrowserProcess retains the diagnostics from a detached browser until the
// session either becomes ready or reports why the child exited.
type BrowserProcess struct {
	PID         int
	Path        string
	UserDataDir string
	done        <-chan error
	stderr      *lockedBuffer
}

func (p *BrowserProcess) exited() (bool, error) {
	if p == nil || p.done == nil {
		return false, nil
	}
	select {
	case err := <-p.done:
		return true, err
	default:
		return false, nil
	}
}

func (p *BrowserProcess) stderrText() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	return strings.TrimSpace(p.stderr.String())
}

type lockedBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 64 << 10
	n := len(p)
	b.b = append(b.b, p...)
	if len(b.b) > limit {
		b.b = append([]byte(nil), b.b[len(b.b)-limit:]...)
	}
	return n, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.b)
}

// LaunchBrowser starts the resolved browser and retains its stderr for the
// session readiness check. Linux root gets Chromium's own --no-sandbox flag;
// any binmgr-provisioned shared libraries are visible to this child only.
func LaunchBrowser(chromePath string, port int, userDataDir string) (*BrowserProcess, error) {
	if port <= 0 {
		port = 9222
	}
	if portInUse(port) {
		return nil, fmt.Errorf("port %d is already in use", port)
	}
	resolved := chromePath
	if resolved == "" {
		var err error
		resolved, err = DefaultResolver().Resolve(context.Background(), "")
		if err != nil {
			return nil, err
		}
	}
	if resolved == "" {
		return nil, fmt.Errorf("Chromium-family browser not found")
	}
	if _, err := os.Stat(resolved); err != nil {
		return nil, fmt.Errorf("Chrome at %q: %w", resolved, err)
	}
	home, _ := os.UserHomeDir()
	resolvedProfile, ephemeral, createdTemp, err := resolveLaunchProfile(port, userDataDir, home, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	userDataDir = resolvedProfile
	libraryPath, err := binmgr.PrepareChromeRuntime(context.Background(), resolved)
	if err != nil {
		if createdTemp {
			_ = os.RemoveAll(userDataDir)
		}
		return nil, fmt.Errorf("prepare Chrome runtime: %w", err)
	}
	cmd := chromeCommand(resolved, port, userDataDir, runtime.GOOS, os.Geteuid(), libraryPath, ephemeral)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	cmd.SysProcAttr = browserDetachedSysProcAttr()
	if err := startBrowserDetached(cmd); err != nil {
		if createdTemp {
			_ = os.RemoveAll(userDataDir)
		}
		return nil, fmt.Errorf("launch chrome: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		if createdTemp {
			_ = os.RemoveAll(userDataDir)
		}
		done <- waitErr
	}()
	return &BrowserProcess{PID: cmd.Process.Pid, Path: resolved, UserDataDir: userDataDir, done: done, stderr: stderr}, nil
}

// AutomationKeychainArgs disables OS credential-store prompts (macOS Keychain
// "Chrome Safe Storage" and Linux Secret Service / KWallet) on ephemeral
// automation profiles. Never pass these to an already-initialized persistent
// profile whose cookies were encrypted with the real OS keychain key.
func AutomationKeychainArgs() []string {
	return []string{
		"--use-mock-keychain",
		"--password-store=basic",
	}
}

// IsInitializedProfile reports whether dir already holds an initialized
// Chromium user-data directory on disk ("Local State" or "Default"), meaning
// its cookies/state may already be encrypted with the host keychain key.
func IsInitializedProfile(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	for _, marker := range []string{"Local State", "Default"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// RejectRealBrowserProfile refuses to launch automation against the operator's
// real desktop Chrome, Chromium, Brave, or Edge user-data directory.
func RejectRealBrowserProfile(dir, home, goos string) error {
	clean := strings.TrimSpace(dir)
	if clean == "" || strings.TrimSpace(home) == "" {
		return nil
	}
	clean = filepath.Clean(clean)
	home = filepath.Clean(home)

	var roots []string
	switch goos {
	case "darwin":
		appSupport := filepath.Join(home, "Library", "Application Support")
		roots = []string{
			filepath.Join(appSupport, "Google", "Chrome"),
			filepath.Join(appSupport, "Google", "Chrome Canary"),
			filepath.Join(appSupport, "Chromium"),
			filepath.Join(appSupport, "BraveSoftware", "Brave-Browser"),
			filepath.Join(appSupport, "Microsoft Edge"),
		}
	case "windows":
		localAppData := filepath.Join(home, "AppData", "Local")
		roots = []string{
			filepath.Join(localAppData, "Google", "Chrome", "User Data"),
			filepath.Join(localAppData, "Chromium", "User Data"),
			filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "User Data"),
			filepath.Join(localAppData, "Microsoft", "Edge", "User Data"),
		}
	default:
		cfg := filepath.Join(home, ".config")
		roots = []string{
			filepath.Join(cfg, "google-chrome"),
			filepath.Join(cfg, "google-chrome-beta"),
			filepath.Join(cfg, "chromium"),
			filepath.Join(cfg, "BraveSoftware", "Brave-Browser"),
			filepath.Join(cfg, "microsoft-edge"),
			filepath.Join(home, "snap", "chromium", "common", "chromium"),
		}
	}

	caseFold := goos == "darwin" || goos == "windows"
	for _, root := range roots {
		if sameOrSubpath(clean, root, caseFold) {
			return fmt.Errorf("refusing to use real browser profile %q for automation; omit --user-data-dir for a throwaway profile or pass a dedicated bashy profile path", dir)
		}
	}
	return nil
}

func sameOrSubpath(candidate, root string, caseFold bool) bool {
	sep := string(filepath.Separator)
	if goosUsesBackslash(root, candidate) {
		candidate = strings.ReplaceAll(candidate, "/", `\`)
		root = strings.ReplaceAll(root, "/", `\`)
		sep = `\`
	}
	candidate = strings.TrimRight(candidate, `/\`)
	root = strings.TrimRight(root, `/\`)
	if caseFold {
		candidate = strings.ToLower(candidate)
		root = strings.ToLower(root)
	}
	return candidate == root || strings.HasPrefix(candidate, root+sep)
}

func goosUsesBackslash(a, b string) bool {
	return strings.Contains(a, `\`) || strings.Contains(b, `\`)
}

func resolveLaunchProfile(port int, explicitDir, home, goos string) (dir string, ephemeral bool, createdTemp bool, err error) {
	if strings.TrimSpace(explicitDir) != "" {
		if err := RejectRealBrowserProfile(explicitDir, home, goos); err != nil {
			return "", false, false, err
		}
		ephemeral = !IsInitializedProfile(explicitDir)
		if err := os.MkdirAll(explicitDir, 0o755); err != nil {
			return "", false, false, fmt.Errorf("user-data-dir: %w", err)
		}
		return explicitDir, ephemeral, false, nil
	}
	defaultDir := DefaultUserDataDir(port)
	if IsInitializedProfile(defaultDir) {
		return defaultDir, false, false, nil
	}
	tmp, err := os.MkdirTemp("", fmt.Sprintf("bashy-browser-probe-%d-*", port))
	if err != nil {
		return "", false, false, fmt.Errorf("temp user-data-dir: %w", err)
	}
	return tmp, true, true, nil
}

func chromeCommand(resolved string, port int, userDataDir, goos string, euid int, libraryPath string, ephemeral bool) *exec.Cmd {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		"--headless=new",
		"--no-first-run",
		"--no-default-browser-check",
	}
	if ephemeral {
		args = append(args, AutomationKeychainArgs()...)
	}
	if goos == "linux" && euid == 0 {
		args = append(args, "--no-sandbox")
	}
	cmd := exec.Command(resolved, args...)
	if libraryPath != "" {
		value := libraryPath
		if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
			value += string(os.PathListSeparator) + inherited
		}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+value)
	}
	return cmd
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
