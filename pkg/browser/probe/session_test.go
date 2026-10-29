package probe

import (
	"context"
	"testing"
)

func TestEnsureRunningKeepsReachableProbe(t *testing.T) {
	m := SessionManager{
		Available: func(context.Context, string) bool { return true },
		Resolve: func(context.Context, string) (string, error) {
			t.Fatal("reachable probe must not resolve a browser")
			return "", nil
		},
		Launch: func(string, int, string) (int, string, error) {
			t.Fatal("reachable probe must not launch a browser")
			return 0, "", nil
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
		Launch: func(path string, port int, profile string) (int, string, error) {
			launched = true
			if path != "/edge" || port != 9333 || profile != "/profile" {
				t.Fatalf("launch(%q, %d, %q)", path, port, profile)
			}
			return 42, path, nil
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
		Launch: func(string, int, string) (int, string, error) {
			t.Fatal("remote endpoint must not launch")
			return 0, "", nil
		},
	}
	if _, err := m.EnsureRunning(context.Background(), SessionConfig{URL: "http://example.test:9222"}); err == nil {
		t.Fatal("expected remote probe refusal")
	}
}
