// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package contain

import (
	"os"
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
// environment, and the spec's resource limits.
type BashyOCI struct {
	// Bin is the bashy launcher; empty = "bashy" on PATH.
	Bin string
	// Image overrides $BASHY_SANDBOX_IMAGE.
	Image string
}

// Argv implements OCIRunner.
func (b BashyOCI) Argv(dir string, argv, env []string, s Spec) ([]string, error) {
	image := strings.TrimSpace(b.Image)
	if image == "" {
		image = strings.TrimSpace(os.Getenv(SandboxImageEnv))
	}
	if image == "" {
		return nil, &UnavailableError{Level: LevelSandbox, Reason: "no sandbox image: set $" + SandboxImageEnv + " to an image that provides bashy"}
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
