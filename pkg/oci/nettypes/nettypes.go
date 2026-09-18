// Package nettypes re-exports Podman/common network types.
package nettypes

import nettypes "go.podman.io/common/libnetwork/types"

type (
	Network           = nettypes.Network
	PortMapping       = nettypes.PortMapping
	PerNetworkOptions = nettypes.PerNetworkOptions
)
