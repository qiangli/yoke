package engine

import (
	"context"
	"testing"
	"time"
)

func TestDefaultSocketPath(t *testing.T) {
	// Just verify it doesn't panic and returns a string.
	path := defaultSocketPath()
	t.Logf("default socket path: %q", path)
}

func TestSocketAvailable(t *testing.T) {
	// Non-existent socket should return false.
	if socketAvailable("/nonexistent/socket.sock") {
		t.Error("expected false for nonexistent socket")
	}
}

func TestWaitForSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// Non-existent socket should timeout quickly.
	err := waitForSocket("/nonexistent/socket.sock", 100*1e6) // 100ms
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestEngineConfig(t *testing.T) {
	cfg := &EngineConfig{
		SocketPath: "/tmp/test.sock",
		DataDir:    t.TempDir(),
	}
	if cfg.SocketPath != "/tmp/test.sock" {
		t.Error("unexpected socket path")
	}
}

func TestDefaultBinCacheDir(t *testing.T) {
	dir := defaultBinCacheDir()
	if dir == "" {
		t.Error("expected non-empty cache dir")
	}
}

// TestNewEngine_UseSystem_NoSocket verifies the escape-hatch refusal:
// when cfg.UseSystem is true and no podman socket can be found,
// NewEngine must return a clean error pointing the user at their own
// podman setup — NOT auto-provision an embedded VM or start an
// in-process service behind their back.
func TestNewEngine_UseSystem_NoSocket(t *testing.T) {
	// Force the no-socket-found branch by handing an explicit
	// nonexistent path AND ensuring the default candidates don't
	// happen to point at a live socket on this dev machine.
	// (defaultSocketPath() probes XDG_RUNTIME_DIR etc; we set it to
	// something definitely empty so no fallback succeeds.)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "")

	cfg := &EngineConfig{
		SocketPath: "/nonexistent/podman.sock",
		UseSystem:  true,
	}

	ctx, cancel := contextWithTimeoutShort(t)
	defer cancel()

	_, err := NewEngine(ctx, cfg)
	if err == nil {
		t.Fatal("NewEngine should fail in useSystem mode when no socket is available")
	}
	msg := err.Error()
	for _, want := range []string{"system mode", "--use-system-binaries"} {
		if !engineErrContains(msg, want) {
			t.Errorf("error should mention %q to make remediation clear; got: %v", want, err)
		}
	}
}

// Helpers — local-only to keep this test self-contained without
// pulling in fmt/strings just for substring matching.
func engineErrContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func contextWithTimeoutShort(t *testing.T) (ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 2*time.Second)
}
