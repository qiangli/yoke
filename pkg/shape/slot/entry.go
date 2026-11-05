package slot

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EntryEnvironmentKey binds one process launch to its explicitly selected image.
// It is a recursion/preview marker within the trusted local OS-account boundary,
// not a credential or permission to activate an unverified candidate.
const EntryEnvironmentKey = "BASHY_SHAPE_ENTRY"

func canonicalExecutable(path string) (string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	if resolved, e := filepath.EvalSymlinks(p); e == nil {
		p = resolved
	}
	return filepath.Clean(p), nil
}

// EntryEnvironment pins this launch to path. Descendant invocations of a
// different executable do not match the marker and resolve the host slot anew.
func EntryEnvironment(path string, env []string) ([]string, error) {
	p, e := canonicalExecutable(path)
	if e != nil {
		return nil, e
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == EntryEnvironmentKey || (runtime.GOOS == "windows" && strings.EqualFold(key, EntryEnvironmentKey))) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, EntryEnvironmentKey+"="+p), nil
}

// PinnedEntry reports whether the current environment explicitly selected this
// image. Preview launchers use the same marker as active-slot dispatchers.
func PinnedEntry(executable string) bool {
	marker := os.Getenv(EntryEnvironmentKey)
	if marker == "" {
		return false
	}
	exe, e := canonicalExecutable(executable)
	if e != nil {
		return false
	}
	marked, e := canonicalExecutable(marker)
	if e != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(exe, marked)
	}
	return exe == marked
}
