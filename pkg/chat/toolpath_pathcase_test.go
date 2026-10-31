package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/nativepath"
)

// pathEntries returns every entry of env that the target OS would read as PATH.
func pathEntries(env []string, windows bool) []string {
	var out []string
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok && nativepath.IsPathName(name, windows) {
			out = append(out, kv)
		}
	}
	return out
}

// The sprint 526 / story 92f9dc4c defect, GOOS-independent. A Windows block
// spells the variable "Path". toolPathEnv matched the literal "PATH=", missed
// it, and appended a second entry holding only the per-user bin homes; os/exec
// dedups a Windows child's environment case-insensitively keeping the LAST
// entry, so the genie child started with a PATH of nothing but those homes and
// its `outpost` exec failed with `executable file not found in %PATH%`.
//
// The child must get exactly one PATH entry, in the launcher's spelling, that
// still begins with every directory the launcher itself used.
func TestToolPathEnvEditsAWindowsSpelledPathInPlace(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("DHNT_BIN_DIR", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	const launcherPath = `C:\Users\liqia\s526-op\cand3;C:\Users\liqia\AppData\Local\outpost;C:\Windows\System32`
	base := []string{"HOME=" + home, "Path=" + launcherPath, "PATHEXT=.COM;.EXE"}
	out := toolPathEnvFor(base, true)

	got := pathEntries(out, true)
	if len(got) != 1 {
		t.Fatalf("child env carries %d PATH entries %q, want exactly one — os/exec keeps only the last", len(got), got)
	}
	if !strings.HasPrefix(got[0], "Path="+launcherPath+";") {
		t.Fatalf("PATH entry = %q, want the launcher's own dirs first under the block's spelling", got[0])
	}
	if want := filepath.Join(home, ".local", "bin"); !strings.Contains(got[0], ";"+want) {
		t.Errorf("PATH entry = %q, want the user bin home %q appended", got[0], want)
	}
	if out[1] != got[0] {
		t.Errorf("PATH was not edited in place: %q", out)
	}
}

// Both spellings at once — a shell exported PATH over a block that already
// had Path — must collapse to the one the launcher itself read (the first).
func TestToolPathEnvCollapsesCaseVariants(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("DHNT_BIN_DIR", "")
	base := []string{`Path=C:\orig`, "FOO=1", `PATH=C:\exported`}
	out := toolPathEnvFor(base, true)
	got := pathEntries(out, true)
	if len(got) != 1 || !strings.HasPrefix(got[0], `Path=C:\orig;`) {
		t.Fatalf("PATH entries = %q, want one, built from the first variant", got)
	}
}

// Off Windows "Path" is an unrelated variable: it is left alone and PATH is
// added, exactly as before.
func TestToolPathEnvLeavesUnixPathAlone(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("DHNT_BIN_DIR", "")
	out := toolPathEnvFor([]string{"Path=/other"}, false)
	if out[0] != "Path=/other" || len(pathEntries(out, false)) != 1 || !strings.HasPrefix(out[1], "PATH=") {
		t.Fatalf("unix toolPathEnv = %q", out)
	}
}

// The same shape for the shell-forcing shim: prepend into the existing entry.
func TestForcedShellEnvEditsAWindowsSpelledPathInPlace(t *testing.T) {
	base := []string{`Path=C:\a;C:\b`, "HOME=x", `SHELL=C:\zsh`}
	out := forcedShellEnvFor(base, `C:\bashy.exe`, `C:\shims`, true)
	got := pathEntries(out, true)
	if len(got) != 1 || got[0] != `Path=C:\shims;C:\a;C:\b` {
		t.Fatalf("PATH entries = %q, want one, shim first, spelling kept", got)
	}
	for _, kv := range out {
		if strings.HasPrefix(kv, "SHELL=") {
			t.Fatalf("SHELL must not be set for a Windows child: %q", out)
		}
	}
}

// The first-party CLI is the running binary. A PATH that names ANOTHER install
// of it first (elephant's shell handed `bashy delegate` a PATH with the
// outpost-installed bashy ahead of the candidate being run) must not win: the
// delegate and the genie it launches have to be the same build.
func TestResolveToolBinaryPrefersTheRunningBinaryOverPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no os.Executable:", err)
	}
	name := filepath.Base(exe)
	dir := t.TempDir()
	other := filepath.Join(dir, name)
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".EXE;.CMD")
	tool := strings.TrimSuffix(name, ".exe")
	if got := ResolveToolBinary(tool); got != exe {
		t.Fatalf("ResolveToolBinary(%q) = %q, want the running binary %q (another copy sits first on PATH at %q)", tool, got, exe, other)
	}
}
