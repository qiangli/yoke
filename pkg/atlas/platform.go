// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import "fmt"

// Platform vocabulary: the three operating systems bashy ships for. A
// command's OS list says where it is SUPPORTED; its Partial list names the
// supported OSes where it runs with a documented gap (a flag or mode that
// errors "not supported on windows"). Portable = full support on all three.
//
// Curated, never inferred — every cmds/ package compiles on every GOOS, so
// build tags prove nothing; the truth is in the *_other.go / *_windows.go
// stubs, and each entry below cites the stub it was read from. Windows is
// the OS where the lists must be believed rather than checked from a mac
// (dhnt docs/windows-crossplatform-uniformity.md), so a change here wants a
// run on a real Windows host, not a green cross-compile.
const (
	OSWindows = "windows"
	OSDarwin  = "darwin"
	OSLinux   = "linux"
)

// OSes returns the platform vocabulary, sorted.
func OSes() []string { return []string{OSDarwin, OSLinux, OSWindows} }

var allOS = []string{OSDarwin, OSLinux, OSWindows}

// SupportedOn reports whether the entry is supported on os.
func (e Entry) SupportedOn(os string) bool {
	for _, o := range e.OS {
		if o == os {
			return true
		}
	}
	return false
}

// PartialOn reports whether the entry runs on os with a documented gap.
func (e Entry) PartialOn(os string) bool {
	for _, o := range e.Partial {
		if o == os {
			return true
		}
	}
	return false
}

// Portable reports full support on every platform: the command a script can
// use as-is on windows, macOS and linux.
func (e Entry) Portable() bool {
	return len(e.OS) == len(allOS) && len(e.Partial) == 0
}

// classifyPlatforms stamps OS and Partial on every entry: everything is
// supported everywhere unless a pass below says otherwise. Runs before the
// alias pass so aliases inherit.
func classifyPlatforms() {
	for n, e := range tools {
		e.OS = append([]string(nil), allOS...)
		tools[n] = e
	}
	for n, e := range verbs {
		e.OS = append([]string(nil), allOS...)
		verbs[n] = e
	}

	// --- unsupported: the whole command errors on the platform --------------
	// cmds/ps/process_other.go: "live process inspection is supported only on
	// Linux"; cmds/chcon/chcon_other.go: SELinux contexts.
	osOnly([]string{OSLinux}, "ps", "chcon")
	// !unix stubs that refuse the command outright: no POSIX uid/gid (chgrp
	// chown), no mode bits (chmod), no FIFO/special files (mkfifo mknod), no
	// scheduling priorities (nice renice), no crontab install (crontab), no
	// terminal messaging (talk), no system log sink (logger), no login name
	// probe (logname).
	osOnly([]string{OSDarwin, OSLinux},
		"chgrp", "chmod", "chown", "mkfifo", "mknod", "nice", "renice",
		"crontab", "talk", "logger", "logname")
	// Bin-managed externals are declared, never defaulted (externalPlatforms).
	declareExternalPlatforms()
	// bashy's engines_windows.go: "bashy ollama: not supported in the Windows
	// engine build".
	osOnly([]string{OSDarwin, OSLinux}, "ollama")

	// --- partial: runs, with a documented gap on that platform --------------
	// Each name cites the stub whose error message names the gap.
	partialOn(OSWindows,
		"more",    // tty_windows.go: interactive terminal mode
		"stty",    // stty_windows.go: most settings
		"sync",    // sync_windows.go: whole-system sync (file operands work)
		"kill",    // signal_other.go: process groups
		"pax",     // fifo/preserve/link stubs: FIFOs, ownership, link times
		"find",    // times_windows.go: -ctime
		"xargs",   // xargs_tty_windows.go: -p (interactive)
		"pr",      // tty_windows.go: /dev/tty
		"stat",    // statfs_other.go: file-system fields
		"pathchk", // limits_other.go: PATH_MAX / NAME_MAX queries
		"touch",   // no_deref_other.go: --no-dereference on a symlink
		"cp",      // special_other.go / link_times_other.go: special files, link times
		"mv",      // special_other.go / owner_other.go: special files, ownership
		"at",      // access_other.go / umask_other.go: access checks, umask
		"batch",   // same as at
		"uptime",  // uptime_windows.go: reduced probe
		"tty",     // tty_windows.go: reduced pathname lookup
	)
	partialOn(OSDarwin,
		"renice", // prio_unix_libc.go: process-group and user adjustments
	)
}

// ExternalPlatform is the declared platform support of a bin-managed external
// (a managed-external / provisioner verb or tool, or a declarative-registry
// CLI). Externals are NEVER defaulted: bashy does not build them, so the only
// evidence that a Windows build exists is the upstream's release asset — and
// WindowsAsset cites one such name in the exact form the package's own
// AssetMatch accepts (platform_external_test.go feeds it back to the matcher).
// A URL-template download cites the template's goos slot instead.
type ExternalPlatform struct {
	OS           []string
	WindowsAsset string // a release asset / URL the package resolves on windows; "" = not on windows
}

// externalPlatforms is the declaration table. Adding a managed external
// without a row here panics at init (see classifyPlatforms).
var externalPlatforms = map[string]ExternalPlatform{
	// pinned POSIX providers — built from C upstream source on the host;
	// cmds/posixproviders: "man 2.12.0 is not supported on windows"
	"ar": {OS: []string{OSDarwin, OSLinux}}, "ctags": {OS: []string{OSDarwin, OSLinux}},
	"ex": {OS: []string{OSDarwin, OSLinux}}, "localedef": {OS: []string{OSDarwin, OSLinux}},
	"lp": {OS: []string{OSDarwin, OSLinux}}, "m4": {OS: []string{OSDarwin, OSLinux}},
	"man": {OS: []string{OSDarwin, OSLinux}}, "nm": {OS: []string{OSDarwin, OSLinux}},
	"strip": {OS: []string{OSDarwin, OSLinux}}, "vi": {OS: []string{OSDarwin, OSLinux}},
	"posix-providers": {OS: []string{OSDarwin, OSLinux}},
	// cmds/why: witr-<os>-<arch>(.zip on windows)
	"why": {OS: allOS, WindowsAsset: "witr-windows-amd64.zip"},
	// managed externals (external/<pkg>, GitHub releases or URL templates)
	"act":        {OS: allOS, WindowsAsset: "act_Windows_x86_64.zip"},
	"act-runner": {OS: allOS, WindowsAsset: "https://dl.gitea.com/act_runner/{version}/act_runner-{version}-windows-amd64.exe"},
	"gh":         {OS: allOS, WindowsAsset: "gh_2.0.0_windows_amd64.zip"},
	"git":        {OS: allOS, WindowsAsset: "pure-Go bashy git (coreutils/git); no download"},
	"helm":       {OS: allOS, WindowsAsset: "https://get.helm.sh/helm-{version}-windows-amd64.tar.gz"},
	"kopia":      {OS: allOS, WindowsAsset: "kopia-0.0.0-windows-x64.zip"},
	"kubectl":    {OS: allOS, WindowsAsset: "https://dl.k8s.io/release/{version}/bin/windows/amd64/kubectl.exe"},
	"loom":       {OS: allOS, WindowsAsset: "gitea-1.0.0-windows-4.0-amd64.exe"},
	"mirror":     {OS: allOS, WindowsAsset: "pure-Go orchestration over rclone (pkg/mirror); no download"},
	"rclone":     {OS: allOS, WindowsAsset: "rclone-v1.0.0-windows-amd64.zip"},
	"seaweedfs":  {OS: allOS, WindowsAsset: "windows_amd64.tar.gz"},
	"zot":        {OS: allOS, WindowsAsset: "zot-windows-amd64"},
	// toolchain provisioners (external/<lang>): each resolves a windows build;
	// git-scm IS the windows path (git-for-windows MinGit; unix uses system git)
	"cargo": {OS: allOS, WindowsAsset: "rustup-init.exe (external/rust)"}, "rustc": {OS: allOS, WindowsAsset: "rustup-init.exe (external/rust)"},
	"rustup": {OS: allOS, WindowsAsset: "rustup-init.exe (external/rust)"}, "rust": {OS: allOS, WindowsAsset: "rustup-init.exe (external/rust)"},
	"clang": {OS: allOS, WindowsAsset: "zig-windows-x86_64 (external/zigcc)"}, "zig": {OS: allOS, WindowsAsset: "zig-windows-x86_64 (external/zigcc)"}, "cmake": {OS: allOS, WindowsAsset: "cmake-*-windows-x86_64.zip (external/cmake)"},
	"curl":    {OS: allOS, WindowsAsset: "curl-*_win64-mingw.zip (external/curlbin)"},
	"git-scm": {OS: allOS, WindowsAsset: "MinGit-*-64-bit.zip (external/gitscm)"},
	"go":      {OS: allOS, WindowsAsset: "go*.windows-amd64.zip (external/gotoolchain)"},
	"mise":    {OS: allOS, WindowsAsset: "mise-v0.0.0-windows-x64.zip"},
	"node":    {OS: allOS, WindowsAsset: "node-*-win-x64.zip (external/node)"}, "npm": {OS: allOS, WindowsAsset: "ships with node"},
	"npx": {OS: allOS, WindowsAsset: "ships with node"}, "pnpm": {OS: allOS, WindowsAsset: "via node/corepack"}, "yarn": {OS: allOS, WindowsAsset: "via node/corepack"},
	"python": {OS: allOS, WindowsAsset: "python-build-standalone *-pc-windows-msvc (external/python)"}, "pip": {OS: allOS, WindowsAsset: "ships with python"},
	"uv": {OS: allOS, WindowsAsset: "uv-x86_64-pc-windows-msvc.zip (external/python)"},
	// declarative registry CLIs (external/registry)
	"doctl":  {OS: allOS, WindowsAsset: "doctl-*-windows-amd64.zip"},
	"gcloud": {OS: allOS, WindowsAsset: "vendor installer (PreferHost); google-cloud-cli-windows-x86_64.zip"},
	"rg":     {OS: allOS, WindowsAsset: "ripgrep-*-x86_64-pc-windows-msvc.zip"},
	"tofu":   {OS: allOS, WindowsAsset: "tofu_*_windows_amd64.zip"},
}

// ExternalPlatforms returns the declared platform support of a bin-managed
// external by name — for the embedder's registry-derived entries, which are
// not in the tables.
func ExternalPlatforms(name string) (ExternalPlatform, bool) {
	e, ok := externalPlatforms[name]
	return e, ok
}

// declareExternalPlatforms applies the table and refuses an undeclared
// external: "we downloaded it" is not evidence it runs on windows.
func declareExternalPlatforms() {
	for _, table := range []map[string]Entry{tools, verbs} {
		for n, e := range table {
			if e.Subclass != SubclassManagedExternal && e.Subclass != SubclassProvisioner {
				continue
			}
			d, ok := externalPlatforms[n]
			if !ok {
				panic(fmt.Sprintf("atlas: managed external %q has no platform declaration (externalPlatforms in platform.go): cite its windows release asset, or declare it unix-only", n))
			}
			e.OS = append([]string(nil), d.OS...)
			table[n] = e
		}
	}
}

func osOnly(oses []string, names ...string) {
	for _, n := range names {
		if e, ok := tools[n]; ok {
			e.OS = append([]string(nil), oses...)
			tools[n] = e
			continue
		}
		if e, ok := verbs[n]; ok {
			e.OS = append([]string(nil), oses...)
			verbs[n] = e
			continue
		}
		panic(fmt.Sprintf("atlas: platform pass names unknown command %q", n))
	}
}

func partialOn(os string, names ...string) {
	for _, n := range names {
		if e, ok := tools[n]; ok {
			if !e.SupportedOn(os) {
				panic(fmt.Sprintf("atlas: %q marked partial on %s but not supported there", n, os))
			}
			e.Partial = append(e.Partial, os)
			tools[n] = e
			continue
		}
		if e, ok := verbs[n]; ok {
			e.Partial = append(e.Partial, os)
			verbs[n] = e
			continue
		}
		panic(fmt.Sprintf("atlas: partial pass names unknown command %q", n))
	}
}
