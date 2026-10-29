// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package contain

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// OCIRunner turns a LevelSandbox run into the host command that launches it
// in a container. contain then runs that command under its ordinary guards
// (procguard, timeout), so the launcher is killed with the run.
type OCIRunner interface {
	// Argv returns the host argv for running argv inside a container whose
	// only mount is dir (read-write) and whose environment is exactly env —
	// the already-scrubbed candidate environment, with dir as its paths.
	Argv(dir string, argv, env []string, s Spec) ([]string, error)
}

// SandboxImageEnv names the image a sandbox island runs in. The image must
// provide the unit's interpreter (bashy) on its PATH.
const SandboxImageEnv = "BASHY_SANDBOX_IMAGE"

// DefaultSandboxImagePrefix is the repository of bashy's own runtime image:
// the FROM-scratch image `bashy self image` builds for the running release,
// tagged with the first 8 hex of its commit. The release pipeline publishes
// it every release (the same image DKS defaults to for its Linux executor),
// so a default install pulls it on first use with no local image present
// and no user action — on every OS bashy oci supports, where the engine
// fetches a missing registry image automatically.
const DefaultSandboxImagePrefix = "ghcr.io/qiangli/bashy-scratch:"

// sandboxReleaseCommit reports the running release's commit for the default
// image tag. It is a variable so tests can pin it; production reads the
// running binary's build info, the same source DKS uses for its own default.
var sandboxReleaseCommit = func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return sandboxCommitFromBuildInfo(bi)
}

// DefaultSandboxImage is the sandbox image of the running release: its own
// bashy build. "" when the build carries no commit to name its own image.
func DefaultSandboxImage() string {
	if c := sandboxReleaseCommit(); len(c) >= 8 {
		return DefaultSandboxImagePrefix + c[:8]
	}
	return ""
}

// ociWorkdir is where the clone is mounted inside the container.
const ociWorkdir = "/work"

// launcherEnv is what the host-side `bashy oci` launcher may inherit: enough
// to locate its own home, engine and machine on every OS — never the
// candidate's environment, which travels only as explicit -e values.
var launcherEnv = []string{
	"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SystemRoot",
	"TEMP", "TMP", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"BASHY_HOME", "CONTAINER_HOST", "BASHY_PODMAN_SYSTEM",
}

// BashyOCI routes a sandbox run to `bashy oci run`: bashy's podman engine,
// provisioned by bashy itself on Windows, macOS and Linux. The run gets the
// clone at /work as its only mount, no network, the candidate's scrubbed
// environment, and the spec's resource limits. The image needs no setup:
// Image, then $BASHY_SANDBOX_IMAGE, then the running release's own image,
// which the engine pulls on first use.
type BashyOCI struct {
	// Bin is the bashy launcher; empty = "bashy" on PATH.
	Bin string
	// Image overrides $BASHY_SANDBOX_IMAGE and the release default.
	Image string
}

// Argv implements OCIRunner.
func (b BashyOCI) Argv(dir string, argv, env []string, s Spec) ([]string, error) {
	image := strings.TrimSpace(b.Image)
	if image == "" {
		image = strings.TrimSpace(os.Getenv(SandboxImageEnv))
	}
	if image == "" {
		image = DefaultSandboxImage()
	}
	if image == "" {
		return nil, &UnavailableError{Level: LevelSandbox, Reason: "no sandbox image: this build carries no commit to name its own image — set $" + SandboxImageEnv + " to an image that provides bashy"}
	}
	bin := b.Bin
	if bin == "" {
		bin = "bashy"
	}
	out := []string{bin, "oci", "run", "--rm", "-i", "--network=none",
		"-v", dir + ":" + ociWorkdir, "-w", ociWorkdir}
	if s.MaxRSSBytes > 0 {
		out = append(out, "--memory", strconv.FormatUint(s.MaxRSSBytes, 10))
	}
	if s.MaxProcs > 0 {
		out = append(out, "--pids-limit", strconv.Itoa(s.MaxProcs))
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" {
			continue // the image's own PATH finds its interpreter
		}
		out = append(out, "-e", k+"="+containerPath(containerPath(v, dir), s.Dir))
	}
	out = append(out, image)
	return append(out, argv...), nil
}

// sandboxCommitFromBuildInfo is the commit a binary was built from: the
// full vcs.revision stamped by the go command, else the 12-hex suffix of a
// Go pseudo-version main module, else "". It mirrors DKS's release identity
// (external/dks/userland_run.go), the same running release whose own image
// names the sandbox default.
func sandboxCommitFromBuildInfo(bi *debug.BuildInfo) string {
	if bi == nil {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && isSandboxHex(s.Value) {
			return s.Value
		}
	}
	v, _, _ := strings.Cut(bi.Main.Version, "+")
	i := strings.LastIndexByte(v, '-')
	if i < 0 {
		return ""
	}
	rev, stamp := v[i+1:], v[:i]
	j := strings.LastIndexAny(stamp, "-.")
	if j < 0 || len(rev) != 12 || !isSandboxHex(rev) {
		return ""
	}
	if ts := stamp[j+1:]; len(ts) != 14 || strings.Trim(ts, "0123456789") != "" {
		return ""
	}
	return rev
}

func isSandboxHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// containerPath rewrites a host path under dir to its mount inside the
// container; other values pass through unchanged.
func containerPath(v, dir string) string {
	if dir == "" {
		return v
	}
	if v == dir {
		return ociWorkdir
	}
	for _, sep := range []string{"/", `\`} {
		if strings.HasPrefix(v, dir+sep) {
			return ociWorkdir + "/" + strings.ReplaceAll(v[len(dir)+1:], `\`, "/")
		}
	}
	return v
}
