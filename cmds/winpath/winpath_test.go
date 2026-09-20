// Behavioral provenance — expected values are DERIVED FROM DOCUMENTED
// upstream behavior; no upstream source was ported (upstream cygpath is
// GPLv3+, wslpath is closed source, and this package must stay a clean
// adapter over BSD-3-Clause mvdan.cc/sh/v3/pathconv):
//
//   - wslpath: Microsoft WSL's built-in converter, shipped in /init since
//     WSL build 17046 (Microsoft WSL release notes,
//     https://learn.microsoft.com/en-us/windows/wsl/release-notes#build-17046
//     — "Added wslpath", with the C:\ → /mnt/c mapping and the -a/-u/-w/-m
//     option surface shown by `wslpath -h`; default is -u, one NAME operand).
//   - cygpath: Cygwin User's Guide, cygpath(1),
//     https://cygwin.com/cygwin-ug-net/cygpath.html (documentation of
//     GPLv3+ winsup/utils/cygpath.cc; manual consulted, source not) —
//     -u/-w/-m output forms (default -u), -p path-list conversion
//     (':' ↔ ';'), several FILE operands converted one per line, and the
//     "can't convert empty path" refusal. The /c/x (rather than
//     /cygdrive/c/x) unix drive spelling follows the MSYS2/Git-for-Windows
//     configuration of the same tool (msys2-runtime, cygdrive prefix "/"),
//     which is the spelling bashy's shell speaks.
//
// Documented deviations, inherited from pathconv (the single converter;
// its shell-boundary semantics win): /dev/null → NUL (upstream cygpath
// prints "nul", upstream wslpath errors); /tmp → the host temp directory
// (upstream maps through the cygwin root / WSL mount); a rootful unix
// path outside any drive mount gains the C: drive (upstream uses the
// cygwin root / \\wsl$ spelling); usage errors exit 2 per the GNU/agent
// contract where the upstreams exit 1.
package winpathcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"mvdan.cc/sh/v3/pathconv"
)

func runTool(t *testing.T, cmd *tool.Tool, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")}}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

// pinTempDir makes the /tmp mapping deterministic across hosts.
func pinTempDir(t *testing.T) {
	t.Helper()
	old := pathconv.TempDir
	pathconv.TempDir = func() string { return `C:\Temp` }
	t.Cleanup(func() { pathconv.TempDir = old })
}

// Both commands are registered under their familiar names, which is what
// makes them reachable through cmds/all: the multicall, the in-process
// Bash ExecHandler and the MCP surface all resolve via this registry.
func TestRegistered(t *testing.T) {
	for _, name := range []string{"wslpath", "cygpath"} {
		if tool.Lookup(name) == nil {
			t.Errorf("tool %q is not registered", name)
		}
	}
}

func TestWslpathConvert(t *testing.T) {
	pinTempDir(t)
	tests := []struct {
		args []string
		want string
	}{
		// default -u: Windows → WSL (release-notes example shape C:\ → /mnt/c)
		{[]string{`C:\Users\x`}, "/mnt/c/Users/x"},
		{[]string{`c:\users`}, "/mnt/c/users"},
		{[]string{`C:/Users/x`}, "/mnt/c/Users/x"},
		{[]string{`C:\`}, "/mnt/c/"},
		{[]string{"-u", `D:\data`}, "/mnt/d/data"},
		// a WSL path is already in the target spelling
		{[]string{"/mnt/c/Users/x"}, "/mnt/c/Users/x"},
		// -w: WSL → Windows
		{[]string{"-w", "/mnt/c/Users/x"}, `C:\Users\x`},
		{[]string{"-w", "/mnt/c"}, `C:\`},
		{[]string{"-w", "/c/x"}, `C:\x`},
		{[]string{"-w", `C:\already\native`}, `C:\already\native`},
		// -m: like -w with forward slashes
		{[]string{"-m", "/mnt/c/Users/x"}, "C:/Users/x"},
		// pathconv shell-boundary operands (deviation: upstream wslpath
		// resolves /tmp inside the distro and errors on /dev/null)
		{[]string{"-w", "/tmp"}, `C:\Temp`},
		{[]string{"-w", "/tmp/a.txt"}, `C:\Temp\a.txt`},
		{[]string{"-w", "/dev/null"}, "NUL"},
	}
	for _, tc := range tests {
		out, errb, code := runTool(t, wslpathCmd, tc.args...)
		if code != 0 || errb != "" || out != tc.want+"\n" {
			t.Errorf("wslpath %v: code=%d out=%q err=%q, want %q", tc.args, code, out, errb, tc.want)
		}
	}
}

func TestCygpathConvert(t *testing.T) {
	pinTempDir(t)
	tests := []struct {
		args []string
		want string
	}{
		// default -u: Windows → unix, MSYS/Git-Bash /c spelling
		{[]string{`C:\Program Files\Git`}, "/c/Program Files/Git"},
		{[]string{`c:/users/x`}, "/c/users/x"},
		{[]string{"-u", `D:\`}, "/d/"},
		// relative paths only swap separators (cygpath -w foo/bar → foo\bar)
		{[]string{`foo\bar`}, "foo/bar"},
		{[]string{"-w", "foo/bar"}, `foo\bar`},
		// -w / -m: unix → Windows
		{[]string{"-w", "/c/Users/x"}, `C:\Users\x`},
		{[]string{"-w", "/mnt/c/Users/x"}, `C:\Users\x`},
		{[]string{"-m", "/c/Users/x"}, "C:/Users/x"},
		{[]string{"--windows", "/c/x"}, `C:\x`},
		// UNC passthrough: //server/share is already native, only separators
		{[]string{"-w", "//server/share"}, `\\server\share`},
		{[]string{"-m", "//server/share"}, "//server/share"},
		// pathconv shell-boundary operands (deviation: upstream prints
		// "nul" and maps /tmp under the cygwin root)
		{[]string{"-w", "/dev/null"}, "NUL"},
		{[]string{"-w", "/tmp/x"}, `C:\Temp\x`},
	}
	for _, tc := range tests {
		out, errb, code := runTool(t, cygpathCmd, tc.args...)
		if code != 0 || errb != "" || out != tc.want+"\n" {
			t.Errorf("cygpath %v: code=%d out=%q err=%q, want %q", tc.args, code, out, errb, tc.want)
		}
	}
}

// cygpath(1): "output each FILE argument" — one converted path per line.
func TestCygpathMultipleOperands(t *testing.T) {
	out, errb, code := runTool(t, cygpathCmd, "-w", "/c/a", "/c/b")
	if code != 0 || errb != "" || out != "C:\\a\nC:\\b\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
}

// cygpath(1) -p: convert a PATH-style list (':' lists ↔ ';' lists).
func TestCygpathPathList(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-p", "-w", "/c/a:/c/b"}, `C:\a;C:\b`},
		{[]string{"-p", "-m", "/c/a:/c/b"}, "C:/a;C:/b"},
		{[]string{"-p", "-u", `C:\a;D:\b`}, "/c/a:/d/b"},
		// an element outside the drive spellings passes through unchanged
		// (deviation: upstream maps it through the cygwin root)
		{[]string{"-p", "-w", "/usr/bin:/c/x"}, `/usr/bin;C:\x`},
	}
	for _, tc := range tests {
		out, errb, code := runTool(t, cygpathCmd, tc.args...)
		if code != 0 || errb != "" || out != tc.want+"\n" {
			t.Errorf("cygpath %v: code=%d out=%q err=%q, want %q", tc.args, code, out, errb, tc.want)
		}
	}
}

// Failure surface: refusals are loud and exit 2 (agent contract; upstream
// exits 1). The empty-path message follows cygpath(1)'s wording.
func TestUsageErrors(t *testing.T) {
	cases := []struct {
		cmd     *tool.Tool
		args    []string
		wantErr string
	}{
		{wslpathCmd, []string{}, "expected exactly one NAME"},
		{wslpathCmd, []string{"a", "b"}, "expected exactly one NAME"},
		{wslpathCmd, []string{""}, "can't convert empty path"},
		{wslpathCmd, []string{"-u", "-w", "/c/x"}, "mutually exclusive"},
		{wslpathCmd, []string{"-z", "x"}, "unknown"},
		{cygpathCmd, []string{}, "missing operand"},
		{cygpathCmd, []string{""}, "can't convert empty path"},
		{cygpathCmd, []string{"-w", "-m", "/c/x"}, "mutually exclusive"},
		{cygpathCmd, []string{"-z", "x"}, "unknown"},
	}
	for _, tc := range cases {
		out, errb, code := runTool(t, tc.cmd, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errb, tc.wantErr) {
			t.Errorf("%s %v: code=%d out=%q err=%q, want exit 2 with %q", tc.cmd.Name, tc.args, code, out, errb, tc.wantErr)
		}
	}
}

// Upstream options outside the first-hour surface fail loudly by name,
// never silently approximate (agent contract).
func TestNotSupportedOptions(t *testing.T) {
	cases := []struct {
		cmd  *tool.Tool
		args []string
	}{
		{wslpathCmd, []string{"-a", "/c/x"}},      // wslpath -h: force absolute
		{cygpathCmd, []string{"-a", "/c/x"}},      // cygpath(1) --absolute
		{cygpathCmd, []string{"-t", "dos", "x"}},  // cygpath(1) --type
		{cygpathCmd, []string{"-f", "list", "x"}}, // cygpath(1) --file
	}
	for _, tc := range cases {
		out, errb, code := runTool(t, tc.cmd, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errb, "not supported") {
			t.Errorf("%s %v: code=%d out=%q err=%q, want exit 2 'not supported'", tc.cmd.Name, tc.args, code, out, errb)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, cmd := range []*tool.Tool{wslpathCmd, cygpathCmd} {
		out, _, code := runTool(t, cmd, "--help")
		if code != 0 || !strings.Contains(out, "Usage: "+cmd.Name) {
			t.Errorf("%s --help: code=%d out=%q", cmd.Name, code, out)
		}
	}
}
