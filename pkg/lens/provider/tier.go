package provider

import (
	"github.com/qiangli/yoke/pkg/ref"
)

// S18 tiers: one locator grammar reaches every tier with the same views.
// TierOf and TransportOf are pure functions of the locator: they consult no
// environment, no config file and no process, so naming a tier or transport
// can never leak a credential. Anything resolved from the environment (tokens,
// kubeconfigs, ambient contexts) stays out of these strings by construction.
//
// The six tiers are local, workspace, sandbox, sphere, k8s and cloud. A
// @host scope is the sphere tier (a directory tree on a sphere peer); the
// sphere: scheme is the same tier over the spacegraph transport.

// TierOf resolves loc to its S18 tier. Unrecognized schemes or scopes report
// "unknown" rather than guessing.
func TierOf(loc ref.Locator) string {
	switch loc.ScopeKind {
	case "ws":
		return "workspace"
	case "sandbox":
		return "sandbox"
	case "host":
		return "sphere"
	case "k8s":
		return "k8s"
	case "cloud":
		return "cloud"
	case "":
	default:
		return "unknown"
	}
	switch loc.Scheme {
	case "sphere":
		return "sphere"
	case "k8s":
		return "k8s"
	case "cloud":
		return "cloud"
	default:
		return "local"
	}
}

// TransportOf names the mechanism that serves loc: local reads, the podman,
// kubectl or doctl CLIs, outpost sftp to a sphere peer, or the spacegraph
// store. Unrecognized schemes or scopes report "unknown".
func TransportOf(loc ref.Locator) string {
	switch loc.ScopeKind {
	case "host":
		return "outpost-sftp"
	case "k8s":
		return "kubectl"
	case "cloud":
		return "doctl"
	case "ws", "sandbox", "":
	default:
		return "unknown"
	}
	switch loc.Scheme {
	case "oci":
		return "podman"
	case "k8s":
		return "kubectl"
	case "cloud":
		return "doctl"
	case "sphere":
		return "spacegraph"
	default:
		return "local"
	}
}
