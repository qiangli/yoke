package loom

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Hook execution — why loom provisions a `bash` NAME for Gitea's hooks.
//
// Gitea writes a shell hook into every repository and git runs it on every
// push. post-receive is what records the push in the activity feed, fires
// webhooks, triggers Actions, and syncs a sync_on_commit push mirror — the
// whole S515.4 "replica shows the new commit within seconds" acceptance hangs
// off that one script. Gitea generates it starting with
//
//	#!/usr/bin/env <SCRIPT_TYPE>      # SCRIPT_TYPE defaults to bash
//
// Windows has no kernel shebang, so Git for Windows emulates one: it reads the
// first line, takes the BASENAME of the interpreter — "env", the argument
// "bash" is dropped — looks that up on PATH and spawns it with the script as
// argv[1]. The msys env.exe then execs the script, the msys runtime honours
// the same shebang, and `env` finally looks up "bash" on PATH.
//
// The MinGit that bashy provisions and sha256-verifies (external/gitscm,
// MinGit-2.55.0.2-64-bit) ships usr/bin/env.exe and usr/bin/sh.exe — and that
// sh.exe IS GNU bash 5.3.015, the whole bash package, 2.4 MB of it — but it
// ships NO bash.exe anywhere in the tree. So the lookup of "bash" found
// nothing and git never ran the hook. A push is served BEFORE its hook runs,
// so the push succeeded while everything downstream of the hook stayed dead:
// the replica never synced, and the repo's activity feed recorded no push at
// all. That is the silent failure S515.4 round 2 hit on the Windows primary
// (docs/s515-4-live-round2.md), where an explicit push_mirrors-sync landed the
// same commit on the replica in 2s.
//
// So loom provides the one name MinGit omits, out of the bash it already
// provisions: usr/bin/bash.exe, hardlinked to usr/bin/sh.exe. Two deliberate
// choices:
//
//   - The name goes NEXT TO sh.exe, not into a shim directory of loom's own. A
//     cygwin/msys program has to stay inside its installation tree: that is
//     where it finds msys-2.0.dll and the mount table that makes
//     `/usr/bin/env` resolve for the NESTED hooks/post-receive.d/* scripts,
//     which carry the same shebang.
//   - It is MinGit's bash rather than bashy's own shell for the same reason.
//     bashy is a bash drop-in and runs these scripts fine, but on Windows it
//     resolves an absolute shebang interpreter through its own virtual POSIX
//     root (BASHY_ROOT), not through the PATH git hands it, so the nested
//     post-receive.d hook is not guaranteed to resolve. Either way no operator
//     installs anything: this bash is downloaded, checksum-verified and cached
//     by bashy itself.
//
// loom then puts that usr/bin (and mingw64/bin) on the PATH it gives Gitea, so
// git's lookup of "env" succeeds however the cmd/git.exe wrapper happens to
// set up its own search path, and the hook's own `cat`/`basename`/`ls` resolve
// to the msys tools the script was written for.

// hookShellName is the on-disk spelling of a shell binary — git looks up the
// interpreter with an .exe suffix on Windows.
func hookShellName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// gitRootFromExe maps the managed git entrypoint <root>/cmd/git.exe back to
// <root>. It returns "" for any other layout, so a changed MinGit tree fails
// loudly instead of silently skipping the hook shell.
func gitRootFromExe(gitExe string) string {
	if gitExe == "" {
		return ""
	}
	dir := filepath.Dir(gitExe)
	if !strings.EqualFold(filepath.Base(dir), "cmd") {
		return ""
	}
	return filepath.Dir(dir)
}

// hookShellDirs are the managed-git directories git must see on PATH to run a
// hook script: usr/bin (env, sh/bash, cat, basename, ls) and mingw64/bin.
func hookShellDirs(root string) []string {
	if root == "" {
		return nil
	}
	return []string{
		filepath.Join(root, "usr", "bin"),
		filepath.Join(root, "mingw64", "bin"),
	}
}

// ensureHookShell makes the "bash" name Gitea's hook shebang asks for
// resolvable inside the managed git's usr/bin, by linking it to the bash that
// ships there as "sh". Idempotent, and safe against a concurrent loom start.
func ensureHookShell(usrBin string) (string, error) {
	if usrBin == "" {
		return "", errors.New("loom: provision hook shell: no managed git tree")
	}
	bash := filepath.Join(usrBin, hookShellName("bash"))
	if usable(bash) {
		return bash, nil
	}
	sh := filepath.Join(usrBin, hookShellName("sh"))
	if !usable(sh) {
		return "", fmt.Errorf("loom: provision hook shell: no shell at %s", sh)
	}
	err := os.Link(sh, bash)
	if err == nil || errors.Is(err, fs.ErrExist) || usable(bash) {
		return bash, nil
	}
	// A filesystem that refuses hardlinks: a copy in the same directory is all
	// the msys loader asks for.
	if cerr := copyShell(sh, bash); cerr != nil {
		if usable(bash) {
			return bash, nil
		}
		return "", fmt.Errorf("loom: provision hook shell: link %s: %v; copy: %w", bash, err, cerr)
	}
	return bash, nil
}

// hookShellEnv is the extra environment the Gitea process needs so git can
// execute the hooks Gitea writes: the managed git's own msys directories, at
// the front of PATH. Appended to the child env, so Go's exec dedup lets it win
// over the inherited PATH (case-insensitively on Windows, where it is "Path").
func hookShellEnv(gitExe string) []string {
	dirs := hookShellDirs(gitRootFromExe(gitExe))
	if len(dirs) == 0 {
		return nil
	}
	path := prependPathDirs(os.Getenv("PATH"), dirs, string(os.PathListSeparator))
	if path == "" {
		return nil
	}
	return []string{"PATH=" + path}
}

// prependPathDirs puts dirs at the front of a PATH value, skipping any already
// present. Comparison is case-insensitive: this runs on Windows paths.
func prependPathDirs(current string, dirs []string, sep string) string {
	existing := strings.Split(current, sep)
	var add []string
	for _, dir := range dirs {
		if dir == "" || containsPathDir(existing, dir) {
			continue
		}
		add = append(add, dir)
	}
	if len(add) == 0 {
		return current
	}
	if current == "" {
		return strings.Join(add, sep)
	}
	return strings.Join(add, sep) + sep + current
}

func containsPathDir(list []string, dir string) bool {
	want := filepath.Clean(dir)
	for _, entry := range list {
		if entry == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(entry), want) {
			return true
		}
	}
	return false
}

func usable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// copyShell copies src to dst through a temporary file in the same directory,
// so a reader never sees a half-written shell.
func copyShell(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
