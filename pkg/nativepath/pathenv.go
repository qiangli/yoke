package nativepath

import (
	"os"
	"strings"
)

// The PATH entry of an environment LIST (what os.Environ returns and what a
// launcher hands exec.Cmd.Env) is not reliably spelled "PATH". A Windows block
// usually spells it "Path" — that is what the OS and a native parent write —
// and os/exec dedups a child's environment case-insensitively on Windows,
// keeping the LAST entry per folded name. So a launcher that edits the list by
// matching the literal "PATH=" prefix misses the real entry, appends a second
// one holding only its additions, and os/exec then lets that second entry
// replace the first: the child starts with a PATH of nothing but the
// launcher's additions. That is how `bashy delegate` on Windows launched a
// genie whose PATH held only the per-user bin homes, so its `outpost` exec
// failed with `executable file not found in %PATH%` while the launcher itself
// had found both bashy and outpost on its own PATH (sprint 526, story
// 92f9dc4c).
//
// These helpers address PATH in a list the way the TARGET OS resolves the name:
// exactly off Windows, case-insensitively on it — and they always leave exactly
// one entry, in place, with its spelling preserved, so os/exec's dedup has
// nothing left to choose between. They take the OS as a parameter and are
// GOOS-independent, so the Windows behaviour is tested on every host.

// IsPathName reports whether name is the PATH variable for the target OS.
func IsPathName(name string, windows bool) bool {
	if windows {
		return strings.EqualFold(name, "PATH")
	}
	return name == "PATH"
}

// PathListSeparator is the PATH element separator for the target OS.
func PathListSeparator(windows bool) string {
	if windows {
		return ";"
	}
	return ":"
}

// LookupPath returns the value of PATH in env as the process itself would read
// it. With several case-variants present (a Windows block can carry both
// "Path=" and "PATH=" when a shell exported one over the other) the FIRST
// wins, which is what GetEnvironmentVariable — and so os.Getenv and
// exec.LookPath — answer for that process; a launcher that used os.Getenv to
// resolve its child must hand the child the same value.
func LookupPath(env []string, windows bool) (string, bool) {
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && IsPathName(name, windows) {
			return value, true
		}
	}
	return "", false
}

// SetPath returns env with PATH set to value: the first PATH entry is
// rewritten in place keeping its spelling, every other case-variant is dropped
// so os/exec's case-insensitive dedup cannot resurrect a stale one, and a
// missing PATH is appended as "PATH=value". env is not modified.
func SetPath(env []string, windows bool, value string) []string {
	out := make([]string, 0, len(env)+1)
	set := false
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && IsPathName(name, windows) {
			if set {
				continue
			}
			out = append(out, name+"="+value)
			set = true
			continue
		}
		out = append(out, kv)
	}
	if !set {
		out = append(out, "PATH="+value)
	}
	return out
}

// Collapse returns env with at most one PATH entry: the first case-variant's
// value, in its place and spelling (see LookupPath for why the first). A list
// with a single PATH entry is returned unchanged.
func Collapse(env []string, windows bool) []string {
	value, ok := LookupPath(env, windows)
	if !ok {
		return env
	}
	n := 0
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok && IsPathName(name, windows) {
			n++
		}
	}
	if n < 2 {
		return env
	}
	return SetPath(env, windows, value)
}

// collapseProcessPath makes the PROCESS block hold one PATH when it was
// started with several case-variants (a Windows child of a shell that
// exported "PATH" while the inherited block spelled it "Path"). Everything in
// this process already reads the first one through os.Getenv; after this,
// os.Environ agrees with it, so a child built from os.Environ cannot inherit
// the other. Windows only; a no-op when the block is already single.
func collapseProcessPath() {
	env := os.Environ()
	if len(Collapse(env, true)) == len(env) {
		return
	}
	value := os.Getenv("PATH")
	for i := 0; i < len(env); i++ {
		if _, ok := os.LookupEnv("PATH"); !ok {
			break
		}
		_ = os.Unsetenv("PATH")
	}
	_ = os.Setenv("PATH", value)
}
