// Package whichcmd implements which(1) (debianutils/GNU-which common
// surface): locate each COMMAND on the search PATH and print the full
// path of the executable that would run. -a prints every match
// instead of the first. Exit status 0 when every COMMAND was found,
// 1 when any was not.
//
// Portions adapted from https://github.com/u-root/u-root
// cmds/core/which/ (BSD-3-Clause).
// Changes: rewired to tool framework; PATH comes from rc.Env (never
// os.Getenv); Windows support via PATHEXT (defaults .com/.exe/.bat/
// .cmd); names containing a separator are checked directly without a
// PATH search; not-found is silent (debianutils behavior).
package whichcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "which",
	Synopsis: "Locate each COMMAND on the search PATH and print the path that would be executed.",
	Usage:    "which [OPTION]... COMMAND...",
}

// Run is wired in init: a literal would create an initialization
// cycle (run's flag-error paths reference cmd).
func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	all := fs.BoolP("all", "a", false, "print all matching pathnames of each argument")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing operand")
	}

	dirs := pathDirs(rc)
	exts := pathExts(rc)
	status := 0
	for _, name := range operands {
		matches := find(rc, dirs, exts, name, *all)
		if len(matches) == 0 {
			status = 1 // silent, like debianutils which
		}
		for _, m := range matches {
			fmt.Fprintln(rc.Out, m)
		}
	}
	return status
}

// find returns the display paths of the executables name resolves to.
// A name containing a directory separator is checked directly (no
// PATH search); otherwise each PATH entry is tried in order.
func find(rc *tool.RunContext, dirs, exts []string, name string, all bool) []string {
	if hasSep(name) {
		c := candidates(rc, name, exts)
		if !all && len(c) > 1 {
			c = c[:1]
		}
		return c
	}
	var out []string
	for _, d := range dirs {
		c := candidates(rc, joinDisplay(d, name), exts)
		if len(c) == 0 {
			continue
		}
		if !all {
			return c[:1]
		}
		out = append(out, c...)
	}
	return out
}

// candidates checks one base path. On unix the base itself must be an
// executable regular file. On Windows the executable suffixes come
// from PATHEXT: the bare name counts only when it already carries an
// extension, then each PATHEXT suffix is tried in order.
func candidates(rc *tool.RunContext, base string, exts []string) []string {
	if len(exts) == 0 {
		if isExecUnix(rc.Path(base)) {
			return []string{base}
		}
		return nil
	}
	var out []string
	if filepath.Ext(base) != "" && isRegular(rc.Path(base)) {
		out = append(out, base)
	}
	for _, e := range exts {
		if p := base + e; isRegular(rc.Path(p)) {
			out = append(out, p)
		}
	}
	return out
}

func isExecUnix(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// pathDirs splits PATH from rc.Env (NOT the process environment).
// An empty entry means the current directory, per POSIX. An unset or
// empty PATH yields no search directories — nothing is found.
func pathDirs(rc *tool.RunContext) []string {
	var raw string
	if runtime.GOOS == "windows" {
		raw = getenvFold(rc, "PATH") // typically spelled "Path" on Windows
	} else {
		raw = rc.Getenv("PATH")
	}
	if raw == "" {
		return nil
	}
	dirs := strings.Split(raw, string(os.PathListSeparator))
	for i, d := range dirs {
		if d == "" {
			dirs[i] = "."
		}
	}
	return dirs
}

// pathExts returns the executable suffixes on Windows (PATHEXT from
// rc.Env, default .com/.exe/.bat/.cmd), nil elsewhere.
func pathExts(rc *tool.RunContext) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	raw := getenvFold(rc, "PATHEXT")
	if raw == "" {
		raw = ".com;.exe;.bat;.cmd"
	}
	var exts []string
	for _, e := range strings.Split(raw, ";") {
		if e == "" {
			continue
		}
		if e[0] != '.' {
			e = "." + e
		}
		exts = append(exts, e)
	}
	return exts
}

// getenvFold is rc.Getenv with case-insensitive keys — Windows
// environment variable names are case-insensitive ("Path", "PATH").
func getenvFold(rc *tool.RunContext, key string) string {
	for i := len(rc.Env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(rc.Env[i], "="); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func hasSep(name string) bool {
	if strings.ContainsRune(name, '/') {
		return true
	}
	return runtime.GOOS == "windows" &&
		(strings.ContainsRune(name, '\\') || filepath.VolumeName(name) != "")
}

// joinDisplay concatenates without Clean so the printed path keeps
// the PATH entry exactly as the user wrote it ("." stays "./name").
func joinDisplay(dir, name string) string {
	if strings.HasSuffix(dir, "/") || (runtime.GOOS == "windows" && strings.HasSuffix(dir, `\`)) {
		return dir + name
	}
	return dir + string(filepath.Separator) + name
}
