package probe

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"
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
		resolved = DetectChrome()
	}
	if resolved == "" {
		return 0, "", fmt.Errorf("Chrome not found on PATH or in standard locations")
	}
	if _, err := os.Stat(resolved); err != nil {
		return 0, "", fmt.Errorf("Chrome at %q: %w", resolved, err)
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return 0, "", fmt.Errorf("user-data-dir: %w", err)
	}
	cmd := exec.Command(resolved,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Start(); err != nil {
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
	for _, p := range platformChromePaths() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func platformChromePaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
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
		}
	}
	return nil
}
