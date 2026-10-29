package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureRunningKeepsReachableProbe(t *testing.T) {
	m := SessionManager{
		Available: func(context.Context, string) bool { return true },
		Resolve: func(context.Context, string) (string, error) {
			t.Fatal("reachable probe must not resolve a browser")
			return "", nil
		},
		Launch: func(string, int, string) (*BrowserProcess, error) {
			t.Fatal("reachable probe must not launch a browser")
			return nil, nil
		},
	}
	got, err := m.EnsureRunning(context.Background(), SessionConfig{URL: "http://127.0.0.1:9222"})
	if err != nil || got.Started {
		t.Fatalf("EnsureRunning = %#v, %v", got, err)
	}
}

func TestEnsureRunningAutoLaunchesResolvedBrowser(t *testing.T) {
	checks := 0
	resolved := false
	launched := false
	m := SessionManager{
		Available: func(context.Context, string) bool {
			checks++
			return checks > 1
		},
		Resolve: func(_ context.Context, explicit string) (string, error) {
			resolved = true
			if explicit != "/edge" {
				t.Fatalf("explicit path = %q", explicit)
			}
			return "/edge", nil
		},
		Launch: func(path string, port int, profile string) (*BrowserProcess, error) {
			launched = true
			if path != "/edge" || port != 9333 || profile != "/profile" {
				t.Fatalf("launch(%q, %d, %q)", path, port, profile)
			}
			return &BrowserProcess{PID: 42, Path: path}, nil
		},
	}
	got, err := m.EnsureRunning(context.Background(), SessionConfig{
		URL: "http://localhost:9333", ChromePath: "/edge", UserDataDir: "/profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved || !launched || !got.Started || got.PID != 42 || got.Port != 9333 {
		t.Fatalf("EnsureRunning = %#v resolved=%t launched=%t", got, resolved, launched)
	}
}

func TestEnsureRunningDoesNotLaunchForRemoteProbe(t *testing.T) {
	m := SessionManager{
		Available: func(context.Context, string) bool { return false },
		Resolve: func(context.Context, string) (string, error) {
			t.Fatal("remote endpoint must be rejected before resolution")
			return "", nil
		},
		Launch: func(string, int, string) (*BrowserProcess, error) {
			t.Fatal("remote endpoint must not launch")
			return nil, nil
		},
	}
	if _, err := m.EnsureRunning(context.Background(), SessionConfig{URL: "http://example.test:9222"}); err == nil {
		t.Fatal("expected remote probe refusal")
	}
}

func TestEnsureRunningSurfacesBrowserStderr(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("exit status 127")
	stderr := &lockedBuffer{}
	_, _ = stderr.Write([]byte("error while loading shared libraries: libnss3.so"))
	m := SessionManager{
		Available: func(context.Context, string) bool { return false },
		Resolve:   func(context.Context, string) (string, error) { return "/managed/chrome", nil },
		Launch: func(path string, _ int, _ string) (*BrowserProcess, error) {
			return &BrowserProcess{PID: 42, Path: path, done: done, stderr: stderr}, nil
		},
	}
	_, err := m.EnsureRunning(context.Background(), SessionConfig{URL: "http://127.0.0.1:9335"})
	if err == nil || !strings.Contains(err.Error(), "libnss3.so") || !strings.Contains(err.Error(), "exit status 127") {
		t.Fatalf("EnsureRunning error = %v", err)
	}
}
