// Package bindings re-exports Podman REST API connection primitives.
package bindings

import "go.podman.io/podman/v6/pkg/bindings"

// NewConnection wraps bindings.NewConnection.
var NewConnection = bindings.NewConnection
