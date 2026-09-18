package spacetime

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// --- core probe implementations --------------------------------------

// probeOSRelease reports a deliberately coarse (bucketed) release: a
// patch upgrade must not move the space-time coordinate.
//   - darwin:  product major ("15")
//   - linux:   distro id + version major ("debian12", "alpine3")
//   - windows: not probed (omitted)
func probeOSRelease() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		v, err := darwinProductVersion()
		if err != nil {
			return "", ErrNotApplicable
		}
		major, _, _ := strings.Cut(v, ".")
		return major, nil
	case "linux":
		f, err := os.Open("/etc/os-release")
		if err != nil {
			return "", ErrNotApplicable
		}
		defer f.Close()
		var id, ver string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, `"`)
			switch k {
			case "ID":
				id = v
			case "VERSION_ID":
				ver, _, _ = strings.Cut(v, ".")
			}
		}
		if sc.Err() != nil || id == "" {
			return "", ErrNotApplicable
		}
		return id + ver, nil
	default:
		return "", ErrNotApplicable
	}
}

// probeLibc distinguishes glibc from musl — linux only.
func probeLibc() (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrNotApplicable
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return "musl", nil
	}
	if m, _ := filepath.Glob("/lib/ld-musl-*"); len(m) > 0 {
		return "musl", nil
	}
	for _, p := range []string{"/lib/x86_64-linux-gnu/libc.so.6", "/lib/aarch64-linux-gnu/libc.so.6", "/lib64/libc.so.6", "/lib/libc.so.6"} {
		if _, err := os.Stat(p); err == nil {
			return "glibc", nil
		}
	}
	return "", ErrNotApplicable
}

func probeContainer() (string, error) {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return "true", nil
		}
	}
	return "false", nil
}

func probeTTY() (string, error) {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return "false", nil
		}
	}
	return "true", nil
}

func probeElevated() (string, error) {
	if runtime.GOOS == "windows" {
		return "", ErrNotApplicable // no admin-token check on windows
	}
	if os.Geteuid() == 0 {
		return "true", nil
	}
	return "false", nil
}

// --- lazy namespace resolvers -----------------------------------------

// toolResolver answers tool.<name>: "absent", a dotted version, or
// "present" when the version is unprobeable. Version probing execs the
// tool once (cached by ProbeSet), bounded by a short timeout.
//
// os/exec note: the repo's no-shell-out rule forbids implementing a
// tool's *behavior* by shelling out. A probe's documented behavior IS
// measuring host binaries (like pkg/binmgr supervising them) — there is
// no pure-Go way to ask an installed tool its version.
type toolResolver struct{}

func (toolResolver) Namespace() string { return "tool" }

// versionArgs maps the common head of tools to their version argv.
// Anything not listed degrades to presence-only (no exec).
var versionArgs = map[string][]string{
	"git": {"--version"}, "go": {"version"}, "node": {"--version"},
	"python3": {"--version"}, "python": {"--version"},
	"docker": {"--version"}, "podman": {"--version"},
	"kubectl": {"version", "--client"}, "helm": {"version", "--short"},
	"make": {"--version"}, "cargo": {"--version"}, "rustc": {"--version"},
	"gh": {"--version"}, "jq": {"--version"}, "curl": {"--version"},
	"tar": {"--version"}, "java": {"-version"},
	"claude": {"--version"}, "codex": {"--version"},
	"opencode": {"--version"}, "aider": {"--version"},
}

var versionRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)

func (toolResolver) Eval(key string) (string, error) {
	path, err := exec.LookPath(key)
	if err != nil {
		return "absent", nil
	}
	args, ok := versionArgs[key]
	if !ok {
		return "present", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if v := versionRe.FindString(string(out)); v != "" {
		return v, nil
	}
	return "present", nil
}

// engineResolver answers engine.<name> (podman, ollama, …): presence of
// a managed/host engine binary. Exec-free (PATH presence); binmgr-tree
// awareness can extend it without changing callers.
type engineResolver struct{}

func (engineResolver) Namespace() string { return "engine" }

func (engineResolver) Eval(key string) (string, error) {
	if _, err := exec.LookPath(key); err == nil {
		return "present", nil
	}
	return "absent", nil
}

// meshResolver reserves the mesh.* namespace (locality, pairing). Empty
// until the sharing phase: every key is not-applicable, so a requires
// clause referencing it fails applicability instead of erroring.
type meshResolver struct{}

func (meshResolver) Namespace() string           { return "mesh" }
func (meshResolver) Eval(string) (string, error) { return "", ErrNotApplicable }
