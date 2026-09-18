// Package kube holds the small pieces shared by the managed kubernetes clients
// `bashy kubectl` and `bashy helm`: cache-first binary resolution and the DKS
// kubeconfig selection (see ResolveKubeconfig) so both tools honor the user's
// standard local kubeconfig by default and target an explicit dhnt plane
// (peer or cloudbox) only when asked.
//
// Both binaries are Apache-2.0 and are DOWNLOADED + exec'd as separate processes
// (never linked), the same trust path as every other binmgr-managed external.
package kube

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const dksKubeconfigRefreshAfter = 50 * time.Minute

// EnvProfile selects which implicit kubeconfig plane a managed kubectl/helm
// targets when $KUBECONFIG is unset. See ResolveKubeconfig.
const EnvProfile = "DKS_PROFILE"

// ProfilePeer targets the local peer-plane control cluster (DKS_PEER_KUBECONFIG,
// default ~/.kube/outpost-control-plane/k3s.yaml). Fails closed if unreadable —
// never falls back to the cloudbox plane.
const ProfilePeer = "peer"

// ProfileCloudbox targets the legacy cloudbox plane (OUTPOST_KUBECONFIG_PATH,
// default ~/.kube/outpost.yaml) and keeps the broker refresh.
const ProfileCloudbox = "cloudbox"

// EnvPeerKubeconfig overrides the peer-plane kubeconfig path.
const EnvPeerKubeconfig = "DKS_PEER_KUBECONFIG"

// ExecName is the on-disk basename for a tool (adds .exe on Windows) — the name
// binmgr.Ensure caches it under (<CacheDir>/<name>/<version>/<execname>).
func ExecName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// CachedBinary returns an already-downloaded tool binary from binmgr's cache
// (newest version wins) so a cache hit needs no network — the hot path for a CLI
// an agent calls repeatedly. Returns "" when nothing is cached.
func CachedBinary(name string) string {
	root, err := binmgr.CacheDir()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(root, name, "*", ExecName(name)))
	sort.Strings(matches) // version dirs sort lexically; good enough, newest-ish last
	for i := len(matches) - 1; i >= 0; i-- {
		if fi, err := os.Stat(matches[i]); err == nil && !fi.IsDir() {
			return matches[i]
		}
	}
	return ""
}

// Resolution is the outcome of ResolveKubeconfig: which kubeconfig path (if any)
// a managed kubectl/helm exec should inject via $KUBECONFIG, and whether that
// path is the cloudbox implicit file eligible for the broker refresh.
type Resolution struct {
	// KUBECONFIG is the path to inject, or "" to leave the tool's own default
	// (its normal $KUBECONFIG / ~/.kube/config resolution) untouched.
	KUBECONFIG string
	// Refresh is true only when KUBECONFIG is the implicit cloudbox file — the
	// one plane whose credentials Outpost's broker can refresh.
	Refresh bool
}

// ResolveKubeconfig decides which kubeconfig (if any) a managed kubectl/helm
// exec should use, in this precedence:
//
//  1. A non-empty explicit $KUBECONFIG always wins: nothing is injected or
//     refreshed, and this function returns a zero Resolution.
//  2. $DKS_PROFILE=peer selects $DKS_PEER_KUBECONFIG, defaulting to
//     ~/.kube/outpost-control-plane/k3s.yaml. The file must exist and be
//     readable or this fails closed — the peer plane never silently falls back
//     to cloudbox.
//  3. $DKS_PROFILE=cloudbox selects $OUTPOST_KUBECONFIG_PATH, defaulting to
//     ~/.kube/outpost.yaml, with the broker refresh enabled. If the path can't
//     be resolved (no $HOME) this also fails closed, since the profile was
//     explicitly requested.
//  4. No profile (the variable is unset, "auto"): if the user's
//     standard ~/.kube/config exists, nothing is injected, so native kubectl/
//     helm honor its current context (the Dragon case: local peer cluster via
//     ~/.kube/config). Only when that standard file is absent does this fall
//     back to the canonical cloudbox outpost.yaml, with refresh enabled —
//     preserving the original zero-config behavior on hosts with no local
//     kubeconfig at all.
//  5. Any set value other than peer or cloudbox, including an explicitly
//     empty value, fails closed rather than silently guessing a plane.
func ResolveKubeconfig() (Resolution, error) {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return Resolution{}, nil // honor the user's explicit choice
	}

	profile, profileSet := os.LookupEnv(EnvProfile)
	profile = strings.TrimSpace(profile)
	if !profileSet {
		if fileExists(standardKubeconfigPath()) {
			return Resolution{}, nil
		}
		path := cloudboxKubeconfigPath()
		if path == "" {
			return Resolution{}, nil
		}
		return Resolution{KUBECONFIG: path, Refresh: true}, nil
	}

	switch profile {
	case ProfilePeer:
		path := peerKubeconfigPath()
		if path == "" || !fileReadable(path) {
			return Resolution{}, fmt.Errorf("kube: %s=peer kubeconfig %q not found or unreadable", EnvProfile, path)
		}
		return Resolution{KUBECONFIG: path}, nil

	case ProfileCloudbox:
		path := cloudboxKubeconfigPath()
		if path == "" {
			return Resolution{}, fmt.Errorf("kube: %s=cloudbox: could not resolve kubeconfig path (no $HOME and no $OUTPOST_KUBECONFIG_PATH)", EnvProfile)
		}
		return Resolution{KUBECONFIG: path, Refresh: true}, nil

	default:
		return Resolution{}, fmt.Errorf("kube: unknown %s=%q (want %q or %q)", EnvProfile, profile, ProfilePeer, ProfileCloudbox)
	}
}

func standardKubeconfigPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}

func peerKubeconfigPath() string {
	if p := os.Getenv(EnvPeerKubeconfig); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "outpost-control-plane", "k3s.yaml")
	}
	return ""
}

func cloudboxKubeconfigPath() string {
	if p := os.Getenv("OUTPOST_KUBECONFIG_PATH"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "outpost.yaml")
	}
	return ""
}

// RefreshKubeconfig refreshes the implicit cloudbox kubeconfig at path through
// Outpost's credential broker when its one-hour bearer token approaches expiry.
// Callers must only pass a Resolution.KUBECONFIG whose Refresh bit is set — the
// peer plane and the user's standard kubeconfig are never touched.
//
// Outpost already owns account credentials and the cloudbox refresh protocol;
// keeping that authority there avoids duplicating secrets or auth logic in
// managed kubectl/helm wrappers. Hosts without Outpost retain the static
// kubeconfig/default-client behavior.
func RefreshKubeconfig(ctx context.Context, stderr io.Writer, path string) {
	if path == "" || fileFresh(path, dksKubeconfigRefreshAfter) {
		return
	}
	outpost, err := exec.LookPath("outpost")
	if err != nil {
		return
	}
	cmd := exec.CommandContext(ctx, outpost, "cluster", "userkubeconfig", "--output", path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "warning: could not refresh DKS kubeconfig through outpost: %v\n", err)
	}
}

// ExecEnvFor is the process env for a kubectl/helm passthrough: the inherited
// env plus KUBECONFIG=path when path is non-empty (as resolved by
// ResolveKubeconfig).
func ExecEnvFor(path string) []string {
	env := os.Environ()
	if path != "" {
		env = append(env, "KUBECONFIG="+path)
	}
	return env
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func fileReadable(p string) bool {
	if !fileExists(p) {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func fileFresh(path string, maxAge time.Duration) bool {
	st, err := os.Stat(path)
	return err == nil && time.Since(st.ModTime()) < maxAge
}
