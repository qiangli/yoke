//go:build linux || freebsd

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	ociEntities "github.com/qiangli/yoke/pkg/oci/entities"
	"github.com/qiangli/yoke/pkg/oci/libpod"
	"github.com/qiangli/yoke/pkg/oci/server"
)

// startServiceInProcess starts the Podman REST API server as an in-process
// goroutine using libpod directly. No external podman binary needed.
// Linux/FreeBSD only — containers run natively via kernel namespaces.
func (e *Engine) startServiceInProcess(ctx context.Context, cfg *EngineConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	dataDir := cfg.DataDir
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".agents", "ycode", "container")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create container data dir: %w", err)
	}

	socketPath := filepath.Join(dataDir, "podman.sock")
	os.Remove(socketPath)

	// Initialize libpod runtime in-process.
	rt, err := libpod.NewRuntime(ctx)
	if err != nil {
		return fmt.Errorf("init libpod runtime: %w", err)
	}

	// Listen on Unix socket.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	// Start the REST API server.
	opts := ociEntities.ServiceOptions{
		Timeout: 0, // no idle timeout
	}
	apiServer, err := server.NewServerWithSettings(rt, ln, opts)
	if err != nil {
		ln.Close()
		return fmt.Errorf("create API server: %w", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.socketPath = socketPath
	e.apiURL = "unix://" + socketPath
	e.inProcess = true

	go func() {
		defer close(e.done)
		if err := apiServer.Serve(); err != nil {
			if sctx.Err() == nil {
				slog.Warn("container: in-process API server exited", "error", err)
			}
		}
	}()

	// Wait for socket to be ready.
	if err := waitForSocket(socketPath, 5*time.Second); err != nil {
		cancel()
		return fmt.Errorf("API server did not start: %w", err)
	}

	e.healthy.Store(true)
	slog.Info("container: started in-process Podman API server", "socket", socketPath)
	return nil
}

// canStartInProcess returns true on Linux/FreeBSD where libpod runs natively.
func canStartInProcess() bool {
	return true
}
