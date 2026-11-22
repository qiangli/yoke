package chat

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// agentShellLauncher resolves the shell's identity, not the current process:
// chat can be running inside a foreman adapter rather than the bashy launcher.
func agentShellLauncher() (string, error) {
	target := os.Getenv("BASHY_SELF")
	if target == "" {
		var err error
		target, err = exec.LookPath("bashy")
		if err != nil {
			return "", err
		}
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if err := validateShellShimTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

// adapterCacheTarget checks both the named path and its symlink destination.
// Check every supported cache location even when an override is active.
func adapterCacheTarget(target string) bool {
	roots := []string{os.Getenv("BASHY_ADAPTER_CACHE")}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		roots = append(roots, filepath.Join(xdg, "bashy", "adapters"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".cache", "bashy", "adapters"))
	}
	canonical := func(path string) string {
		path, _ = filepath.Abs(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return path
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, pair := range [][2]string{{root, target}, {canonical(root), canonical(target)}} {
			base, err := filepath.Abs(pair[0])
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(base, pair[1])
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func validateShellShimTarget(bashy string) error {
	if !filepath.IsAbs(bashy) {
		return fmt.Errorf("shell shim target must be absolute: %q", bashy)
	}
	if strings.HasSuffix(bashy, ".test") {
		return fmt.Errorf("shell shim target must not be a test binary: %q", bashy)
	}
	if strings.Contains(bashy, "go-build") {
		return fmt.Errorf("shell shim target must not be under go-build: %q", bashy)
	}
	if adapterCacheTarget(bashy) {
		return fmt.Errorf("shell shim target must not be under the adapter cache: %q", bashy)
	}
	return nil
}

// WriteShellShim installs a POSIX shell-name wrapper that execs the AgentOS
// entry point by its own name. A symlink named bash/sh selects the plain shell
// route in the multicall binary and silently disables AgentOS middleware.
// Rename replaces legacy symlinks without ever writing through their target.
func WriteShellShim(path, bashy string) error {
	if err := validateShellShimTarget(bashy); err != nil {
		return err
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
