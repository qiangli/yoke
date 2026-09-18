package chat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Resolving an agent CLI must not depend on the launcher's inherited PATH.
//
// `bashy chat` is reached three ways with three different environments: an
// operator's login shell (PATH has everything), a supervised daemon —
// `bashy app serve`, `bashy meet serve`, `foreman serve` — started by a
// service manager, and a spawned agent's own shell-out. Only the first is a
// login shell. A daemon launched by launchd/systemd inherits a service PATH
// that does not include the per-user binary home, so an agent CLI installed
// exactly where the host-install contract says to put it (docs/user-bin-home.md:
// $DHNT_BIN_DIR, default ~/.local/bin) is invisible to it — the failure reads
// `exec: "claude": executable file not found in $PATH` while the operator can
// run `claude` from their own terminal, and the launch dies at the same place
// for every agent bound to that tool.
//
// This is the same defect binmgr.CachedBinary already records on the engine
// side ("callers then fell back to $PATH, which made a service definition's
// PATH load-bearing"); an agent CLI is no more willing to be found by a
// regenerated service PATH than podman was.
//
// So the search path is derived, not inherited: the launcher's PATH first — an
// operator who put a tool somewhere still wins — then the well-known per-user
// binary homes appended, never prepended, so nothing on the real PATH is
// shadowed by a stale copy in ~/bin.

// userBinDirs returns the per-user binary homes searched after $PATH.
//
// $DHNT_BIN_DIR is the host-install contract and comes first. The rest are the
// conventional per-user homes third-party agent CLIs install into (claude's
// installer writes ~/.local/bin; `go install` writes $GOBIN or ~/go/bin), and a
// dhnt host is expected to carry agent CLIs that dhnt did not install.
func userBinDirs() []string {
	var dirs []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		for _, have := range dirs {
			if have == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	add(os.Getenv("DHNT_BIN_DIR"))
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dirs
	}
	add(filepath.Join(home, ".local", "bin"))
	add(filepath.Join(home, "bin"))
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		add(gobin)
	} else if gopath := os.Getenv("GOPATH"); gopath != "" {
		add(filepath.Join(gopath, "bin"))
	} else {
		add(filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// executableFile reports whether p is a runnable regular file.
func executableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}

// lookInUserBins finds tool in the per-user binary homes, honouring the same
// extension rules the OS applies to a bare name.
func lookInUserBins(tool string) string {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = nil
		for _, e := range filepath.SplitList(os.Getenv("PATHEXT")) {
			exts = append(exts, strings.ToLower(e))
		}
		if len(exts) == 0 {
			exts = []string{".exe", ".bat", ".cmd"}
		}
		if filepath.Ext(tool) != "" {
			exts = append([]string{""}, exts...)
		}
	}
	for _, dir := range userBinDirs() {
		for _, ext := range exts {
			p := filepath.Join(dir, tool+ext)
			if executableFile(p) {
				return p
			}
		}
	}
	return ""
}

// ResolveToolBinary returns an absolute path for an agent CLI, or "" when the
// name cannot be resolved anywhere this host knows to look.
//
// A name that already carries a path separator is the caller's explicit choice
// (an operator's `cli.binary: /opt/agents/claude`) and is returned untouched —
// resolution is for bare names only.
func ResolveToolBinary(tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return ""
	}
	if strings.ContainsRune(tool, filepath.Separator) || strings.ContainsRune(tool, '/') {
		return tool
	}
	if p, err := exec.LookPath(tool); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return lookInUserBins(tool)
}

// toolPathEnv returns base with the per-user binary homes appended to PATH.
//
// The child gets the same view its launcher just used. Without this a resolved
// agent starts, then fails the moment it shells out to a sibling CLI that lives
// in the same directory it was itself found in — a launch that half-works is
// harder to diagnose than one that does not start.
func toolPathEnv(base []string) []string {
	dirs := userBinDirs()
	if len(dirs) == 0 {
		return base
	}
	sep := string(os.PathListSeparator)
	out := make([]string, 0, len(base)+1)
	found := false
	for _, kv := range base {
		if !found && strings.HasPrefix(kv, "PATH=") {
			found = true
			cur := kv[len("PATH="):]
			have := map[string]bool{}
			for _, d := range filepath.SplitList(cur) {
				have[d] = true
			}
			var extra []string
			for _, d := range dirs {
				if !have[d] {
					extra = append(extra, d)
				}
			}
			if len(extra) > 0 {
				if cur == "" {
					cur = strings.Join(extra, sep)
				} else {
					cur = cur + sep + strings.Join(extra, sep)
				}
			}
			out = append(out, "PATH="+cur)
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+strings.Join(dirs, sep))
	}
	return out
}

// ErrToolNotFound explains a failed resolution in the terms the operator can
// act on: which tool, and where this host looked.
//
// `executable file not found in $PATH` names neither, and on a daemon whose
// $PATH is not the operator's own it points at the wrong environment entirely.
func ErrToolNotFound(tool string) error {
	dirs := append(filepath.SplitList(os.Getenv("PATH")), userBinDirs()...)
	return fmt.Errorf("agent CLI %q not found (searched %s); install it or set its absolute path as `cli.binary` in the tool's catalog entry",
		tool, strings.Join(dirs, string(os.PathListSeparator)))
}
