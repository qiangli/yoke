// Package selfbin resolves which bashy binary shaping subprocesses launch.
package selfbin

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns configured when set; otherwise the running executable
// (symlinks resolved) when it is a bashy build; otherwise "bashy" on PATH.
func Resolve(configured string) string {
	return resolve(configured, os.Executable)
}

func resolve(configured string, executable func() (string, error)) string {
	if c := strings.TrimSpace(configured); c != "" {
		return c
	}
	exe, err := executable()
	if err != nil || exe == "" {
		return "bashy"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	base := strings.ToLower(filepath.Base(exe))
	base = strings.TrimSuffix(base, ".exe")
	if base != "bashy" {
		return "bashy"
	}
	return exe
}
