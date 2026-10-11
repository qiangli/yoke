// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"os"
	"path/filepath"
)

// serviceDir is the console's private state directory (session key, pairing,
// look). Relocating $BASHY_HOME relocates it, which keeps tests isolated.
func serviceDir() (string, error) {
	if h := os.Getenv("BASHY_HOME"); h != "" {
		return filepath.Join(h, "console"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "console"), nil
}
