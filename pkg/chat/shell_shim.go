package chat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteShellShim installs a POSIX shell-name wrapper that execs the AgentOS
// entry point by its own name. A symlink named bash/sh selects the plain shell
// route in the multicall binary and silently disables AgentOS middleware.
// Rename replaces legacy symlinks without ever writing through their target.
func WriteShellShim(path, bashy string) error {
	if !filepath.IsAbs(bashy) {
		return fmt.Errorf("shell shim target must be absolute: %q", bashy)
	}
	body := []byte("#!/bin/sh\nexec '" + strings.ReplaceAll(bashy, "'", "'\"'\"'") + "' \"$@\"\n")
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, body) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".shell-shim-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Chmod(0755)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
