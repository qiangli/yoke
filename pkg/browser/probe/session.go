package probe

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

type SessionConfig struct {
	URL         string
	ChromePath  string
	UserDataDir string
}

type LaunchResult struct {
	Started     bool
	PID         int
	BrowserPath string
	Port        int
	UserDataDir string
}

// SessionManager owns the probe auto-launch decision. Dependencies are
// explicit so tests use a fake resolver and launcher and never download or
// start a real browser.
type SessionManager struct {
	Available func(context.Context, string) bool
	Resolve   func(context.Context, string) (string, error)
	Launch    func(string, int, string) (int, string, error)
}

func DefaultSessionManager() SessionManager {
	r := DefaultResolver()
	return SessionManager{
		Available: func(ctx context.Context, endpoint string) bool { return New(endpoint).Available(ctx) },
		Resolve:   r.Resolve,
		Launch:    LaunchChrome,
	}
}

func (m SessionManager) EnsureRunning(ctx context.Context, cfg SessionConfig) (LaunchResult, error) {
	endpoint := strings.TrimRight(cfg.URL, "/")
	if endpoint == "" {
		endpoint = DefaultURL
	}
	if m.Available == nil || m.Resolve == nil || m.Launch == nil {
		return LaunchResult{}, fmt.Errorf("probe: incomplete session manager")
	}
	if m.Available(ctx, endpoint) {
		return LaunchResult{}, nil
	}
	port, err := localProbePort(endpoint)
	if err != nil {
		return LaunchResult{}, err
	}
	path, err := m.Resolve(ctx, cfg.ChromePath)
	if err != nil {
		return LaunchResult{}, err
	}
	userDataDir := cfg.UserDataDir
	if userDataDir == "" {
		userDataDir = DefaultUserDataDir(port)
	}
	pid, resolved, err := m.Launch(path, port, userDataDir)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("probe: auto-launch: %w", err)
	}
	result := LaunchResult{Started: true, PID: pid, BrowserPath: resolved, Port: port, UserDataDir: userDataDir}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m.Available(ctx, endpoint) {
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if time.Now().After(deadline) {
			return result, fmt.Errorf("probe: browser launched but %s did not become ready", endpoint)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func localProbePort(endpoint string) (int, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return 0, fmt.Errorf("probe: invalid --probe-url %q", endpoint)
	}
	host := strings.Trim(strings.ToLower(u.Hostname()), "[]")
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		return 0, fmt.Errorf("probe: %s is remote; auto-launch is limited to loopback endpoints", endpoint)
	}
	if u.Port() == "" {
		if u.Scheme == "https" {
			return 443, nil
		}
		return 80, nil
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("probe: invalid port in --probe-url %q", endpoint)
	}
	return port, nil
}

// Stop asks the browser at endpoint to shut itself down through CDP. This is
// portable across Chromium, Edge, and the managed headless shell.
func Stop(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		endpoint = DefaultURL
	}
	if !New(endpoint).Available(ctx) {
		return fmt.Errorf("probe: no browser at %s", endpoint)
	}
	allocCtx, allocStop := chromedp.NewRemoteAllocator(ctx, endpoint)
	defer allocStop()
	cdpCtx, cdpStop := chromedp.NewContext(allocCtx)
	defer cdpStop()
	if err := chromedp.Run(cdpCtx, cdpbrowser.Close()); err != nil {
		return fmt.Errorf("probe: stop %s: %w", endpoint, err)
	}
	return nil
}
