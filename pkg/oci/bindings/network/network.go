// Package network re-exports Podman network bindings.
package network

import "go.podman.io/podman/v6/pkg/bindings/network"

type ListOptions = network.ListOptions

var (
	Create = network.Create
	Remove = network.Remove
	List   = network.List
)
