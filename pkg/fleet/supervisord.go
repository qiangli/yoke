package fleet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SupervisordSocketPath is where bashy supervisord's control socket lives:
// $BASHY_SUPERVISORD_SOCKET, else $BASHY_HOME/service/supervisord.sock,
// else ~/.bashy/service/supervisord.sock. This mirrors
// pkg/service.SupervisordSocket; this package cannot import pkg/service
// (pkg/service imports pkg/fleet), so the resolution is kept here too
// rather than left unavailable to the one caller — EnsureSupervisordRunning
// — that needs to know whether a monitor already answers before starting
// one under the same BASHY_HOME.
func SupervisordSocketPath() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_SUPERVISORD_SOCKET")); p != "" {
		return p
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "service", "supervisord.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".bashy", "service", "supervisord.sock")
}

// SupervisordRunning reports whether something answers the monitor's
// control socket.
func SupervisordRunning() bool {
	conn, err := net.DialTimeout("unix", SupervisordSocketPath(), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// EnsureSupervisordRunning starts bashy supervisord (argv) as a detached
// background process when nothing answers its control socket, and waits
// until something does. The child inherits the current environment, so it
// resolves the same BASHY_HOME (and so the same service ring and control
// socket) the caller is running under. Idempotent: a monitor already
// running is left alone and argv is never touched.
//
// This is the fix for `llm up --service` starting the door itself as a
// second, unsupervised process: the caller enables the unit, then calls
// this instead of starting the door, so supervisord — the one process that
// restarts a crashed unit or brings it back up at boot — is the one that
// starts it.
func EnsureSupervisordRunning(ctx context.Context, argv []string) error {
	if SupervisordRunning() {
		return nil
	}
	if len(argv) == 0 {
		return errors.New("fleet: no supervisord command to start")
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.SysProcAttr = detachSysProcAttr()
	if err := startDetached(c); err != nil {
		return fmt.Errorf("fleet: start bashy supervisord: %w", err)
	}
	_ = c.Process.Release()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if SupervisordRunning() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("fleet: bashy supervisord did not come up within 30s (socket %s)", SupervisordSocketPath())
}
