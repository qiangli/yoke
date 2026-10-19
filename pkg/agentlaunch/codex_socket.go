package agentlaunch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// codexSocketDiagnostic reports a dangling control symlink without touching
// the Codex daemon or its runtime directory.
func codexSocketDiagnostic() error {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(home, "app-server-control", "app-server-control.sock")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex control socket %s points to a missing runtime socket; stop the stale Codex app-server daemon and retry so Codex can recreate it (preserve live runtime directories)", path)
	}
	return nil
}
