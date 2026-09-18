// Package images re-exports Podman image bindings.
package images

import "go.podman.io/podman/v6/pkg/bindings/images"

type RemoveOptions = images.RemoveOptions

var (
	Pull   = images.Pull
	List   = images.List
	Exists = images.Exists
	Remove = images.Remove
	Build  = images.Build
)
