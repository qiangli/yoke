// Package winpathcmd implements the familiar Windows path-spelling
// converters wslpath (WSL) and cygpath (Cygwin/MSYS2/Git-Bash) as thin
// adapters over mvdan.cc/sh/v3/pathconv — the ONE converter the bashy
// shell itself applies at the Windows boundary — so `cygpath -w` and the
// shell agree on every spelling: C:\x, C:/x, /c/x, /mnt/c/x, /tmp,
// /dev/null. The two commands differ only in their unix drive form
// (wslpath speaks /mnt/c/…, cygpath the MSYS/Git-Bash /c/…) and their
// operand arity (wslpath takes one NAME, cygpath any number).
//
// Deviations from upstream are pathconv's shell-boundary semantics, held
// deliberately (one converter, one answer): /dev/null converts to NUL
// (upstream cygpath prints "nul", upstream wslpath errors); /tmp maps to
// the host temp directory rather than a cygwin-root/WSL-VHD path; a
// rootful unix path outside any drive mount (/usr/x) gains the C: drive.
// Conversion is pure string work on any host — nothing is resolved
// against the filesystem, so output is deterministic across GOOS.
package winpathcmd

import (
	"fmt"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"mvdan.cc/sh/v3/pathconv"
)

var wslpathCmd = &tool.Tool{
	Name:     "wslpath",
	Synopsis: "Convert between the Windows and WSL (/mnt/<drive>) path spellings.",
	Usage:    "wslpath [-u|-w|-m] NAME",
}

var cygpathCmd = &tool.Tool{
	Name:     "cygpath",
	Synopsis: "Convert between the Windows and unix (/<drive>, MSYS/Git-Bash) path spellings.",
	Usage:    "cygpath [-u|-w|-m] [-p] NAME...",
}

// Run is wired in init: a literal would create an initialization cycle
// (the run functions' flag-error paths reference their Tool).
func init() {
	wslpathCmd.Run = runWslpath
	cygpathCmd.Run = runCygpath
	tool.Register(wslpathCmd)
	tool.Register(cygpathCmd)
}

type mode int

const (
	modeUnix    mode = iota // -u: unix spelling (the default in both upstreams)
	modeWindows             // -w: native backslash spelling C:\x
	modeMixed               // -m: native drive with forward slashes C:/x
)

func runWslpath(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(wslpathCmd.Name)
	unix := fs.BoolP("unix", "u", false, "translate from a Windows path to a WSL path (default)")
	win := fs.BoolP("windows", "w", false, "translate from a WSL path to a Windows path")
	mixed := fs.BoolP("mixed", "m", false, `like -w, but with '/' instead of '\'`)
	abs := fs.BoolP("absolute", "a", false, "force result to absolute path format (not supported)")
	operands, code := tool.Parse(rc, wslpathCmd, fs, args)
	if code >= 0 {
		return code
	}
	if *abs {
		return tool.NotSupported(rc, wslpathCmd, "-a (force absolute result)")
	}
	m, ok := pickMode(*unix, *win, *mixed)
	if !ok {
		return tool.UsageError(rc, wslpathCmd, "options -u, -w and -m are mutually exclusive")
	}
	if len(operands) != 1 {
		return tool.UsageError(rc, wslpathCmd, "expected exactly one NAME operand, got %d", len(operands))
	}
	if operands[0] == "" {
		return tool.UsageError(rc, wslpathCmd, "can't convert empty path")
	}
	fmt.Fprintln(rc.Out, convertOne(m, true, operands[0]))
	return 0
}

func runCygpath(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cygpathCmd.Name)
	unix := fs.BoolP("unix", "u", false, "print the unix form: /c/x, forward slashes (default)")
	win := fs.BoolP("windows", "w", false, `print the Windows form: C:\x, backslashes`)
	mixed := fs.BoolP("mixed", "m", false, "print the Windows form with forward slashes: C:/x")
	pathList := fs.BoolP("path", "p", false, "NAME is a PATH-style list: convert ':' lists to ';' lists and back")
	abs := fs.BoolP("absolute", "a", false, "output absolute path (not supported)")
	typ := fs.StringP("type", "t", "", "print TYPE form (not supported)")
	file := fs.StringP("file", "f", "", "read paths from FILE (not supported)")
	operands, code := tool.Parse(rc, cygpathCmd, fs, args)
	if code >= 0 {
		return code
	}
	if *abs {
		return tool.NotSupported(rc, cygpathCmd, "-a/--absolute")
	}
	if *typ != "" {
		return tool.NotSupported(rc, cygpathCmd, "-t/--type")
	}
	if *file != "" {
		return tool.NotSupported(rc, cygpathCmd, "-f/--file")
	}
	m, ok := pickMode(*unix, *win, *mixed)
	if !ok {
		return tool.UsageError(rc, cygpathCmd, "options -u, -w and -m are mutually exclusive")
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cygpathCmd, "missing operand")
	}
	for _, op := range operands {
		if op == "" {
			return tool.UsageError(rc, cygpathCmd, "can't convert empty path")
		}
		if *pathList {
			fmt.Fprintln(rc.Out, convertList(m, op))
		} else {
			fmt.Fprintln(rc.Out, convertOne(m, false, op))
		}
	}
	return 0
}

// pickMode maps the three output-form flags to a mode; more than one set
// is a refusal, never a silent guess (agent contract).
func pickMode(unix, win, mixed bool) (mode, bool) {
	n, m := 0, modeUnix
	if unix {
		n++
	}
	if win {
		n++
		m = modeWindows
	}
	if mixed {
		n++
		m = modeMixed
	}
	return m, n <= 1
}

// convertOne converts one path. wsl selects the WSL /mnt/<drive> spelling
// for unix output instead of pathconv's MSYS/Git-Bash /<drive> form.
func convertOne(m mode, wsl bool, path string) string {
	switch m {
	case modeWindows:
		// ToOSMode leaves relative and UNC paths' separators alone; both
		// upstreams print backslashes for -w, and backslashifying the whole
		// result is safe because no native Windows spelling keeps a '/'.
		return strings.ReplaceAll(pathconv.ToOSMode("", path, true), "/", `\`)
	case modeMixed:
		return pathconv.ToSlashMode("", path, true)
	default:
		p := pathconv.FromOSMode(path, true)
		if wsl {
			if drive, rest, ok := pathconv.DrivePath(p); ok {
				return "/mnt/" + string(drive|0x20) + rest
			}
		}
		return p
	}
}

// convertList converts one PATH-style list (cygpath -p). To the Windows
// forms pathconv.NativePathList does the work (':' lists become ';' lists
// with each drive-form element converted); to the unix form each ';'
// element goes through the converter and the list is rejoined with ':'.
func convertList(m mode, list string) string {
	switch m {
	case modeWindows, modeMixed:
		out := pathconv.NativePathList(list)
		if m == modeMixed {
			out = strings.ReplaceAll(out, `\`, "/")
		}
		return out
	default:
		elems := strings.Split(list, ";")
		for i, e := range elems {
			elems[i] = pathconv.FromOSMode(e, true)
		}
		return strings.Join(elems, ":")
	}
}
