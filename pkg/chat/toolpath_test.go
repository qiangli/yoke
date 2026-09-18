package chat

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The defect this file exists for: `bashy app serve` / `meet serve` run under a
// service manager whose PATH is not the operator's login PATH, so an agent CLI
// installed at the host-install contract's own location was invisible and every
// launch died with `exec: "claude": executable file not found in $PATH`.
func TestResolveToolBinaryFindsUserBinHomeOffPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the 0o755 bit is not how Windows decides an executable")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(bin, "fake-agent-cli")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("DHNT_BIN_DIR", "")
	// A service PATH: nothing of the operator's own in it.
	t.Setenv("PATH", "/usr/bin:/bin")

	if got := ResolveToolBinary("fake-agent-cli"); got != tool {
		t.Errorf("ResolveToolBinary = %q, want %q — an agent installed in the user bin home must be launchable from a daemon", got, tool)
	}
}

// $DHNT_BIN_DIR is the host-install contract (docs/user-bin-home.md) and wins
// over the conventional homes.
func TestResolveToolBinaryHonoursDhntBinDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the 0o755 bit is not how Windows decides an executable")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "fake-agent-cli")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DHNT_BIN_DIR", dir)
	t.Setenv("PATH", "/usr/bin:/bin")

	if got := ResolveToolBinary("fake-agent-cli"); got != tool {
		t.Errorf("ResolveToolBinary = %q, want %q", got, tool)
	}
}

// An operator's explicit `cli.binary: /opt/agents/claude` is a decision, not a
// name to resolve — resolution is for bare names only.
func TestResolveToolBinaryPassesAPathThrough(t *testing.T) {
	const explicit = "/opt/agents/claude"
	if got := ResolveToolBinary(explicit); got != explicit {
		t.Errorf("ResolveToolBinary = %q, want the caller's own path %q", got, explicit)
	}
}

// Unresolvable is reported as unresolvable — the caller preflights on it rather
// than handing os/exec a name it will fail on with a $PATH the operator cannot see.
func TestResolveToolBinaryReportsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DHNT_BIN_DIR", "")
	t.Setenv("PATH", t.TempDir())
	if got := ResolveToolBinary("definitely-not-installed-anywhere"); got != "" {
		t.Errorf("ResolveToolBinary = %q, want \"\"", got)
	}
}

// The child gets the same view its launcher used. Appended, never prepended: an
// operator's own PATH order must survive.
func TestToolPathEnvAppendsUserBinHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DHNT_BIN_DIR", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	sep := string(os.PathListSeparator)
	out := toolPathEnv([]string{"FOO=1", "PATH=/usr/bin" + sep + "/bin"})

	var path string
	for _, kv := range out {
		if strings.HasPrefix(kv, "PATH=") {
			path = kv[len("PATH="):]
		}
	}
	dirs := filepath.SplitList(path)
	if len(dirs) < 2 || dirs[0] != "/usr/bin" || dirs[1] != "/bin" {
		t.Fatalf("PATH = %q, want the launcher's own entries first", path)
	}
	want := filepath.Join(home, ".local", "bin")
	if !slicesContain(dirs, want) {
		t.Errorf("PATH = %q, want it to include %q", path, want)
	}
}

// Applied twice — a launcher that already inherited an augmented PATH — must not
// grow the variable on every hop.
func TestToolPathEnvIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DHNT_BIN_DIR", "")

	once := toolPathEnv([]string{"PATH=/usr/bin"})
	twice := toolPathEnv(once)
	if len(once) != len(twice) || once[0] != twice[0] {
		t.Errorf("toolPathEnv is not idempotent:\n once = %q\ntwice = %q", once, twice)
	}
}

// A child env with no PATH at all still gets one.
func TestToolPathEnvAddsPathWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DHNT_BIN_DIR", "")
	out := toolPathEnv([]string{"FOO=1"})
	found := false
	for _, kv := range out {
		if strings.HasPrefix(kv, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Errorf("toolPathEnv = %q, want a PATH entry", out)
	}
}

// The error names the tool and where this host looked. `executable file not
// found in $PATH` names neither, and points at an environment the operator does
// not own when the launcher is a daemon.
func TestErrToolNotFoundNamesTheToolAndTheSearch(t *testing.T) {
	err := ErrToolNotFound("claude")
	msg := err.Error()
	if !strings.Contains(msg, `"claude"`) || !strings.Contains(msg, "searched") || !strings.Contains(msg, "cli.binary") {
		t.Errorf("ErrToolNotFound = %q, want the tool, the search path, and the fix", msg)
	}
}

func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
