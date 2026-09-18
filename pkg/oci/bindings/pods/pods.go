// Package pods re-exports Podman pod bindings.
package pods

import "go.podman.io/podman/v6/pkg/bindings/pods"

type (
	RemoveOptions = pods.RemoveOptions
	ListOptions   = pods.ListOptions
)

var (
	CreatePodFromSpec = pods.CreatePodFromSpec
	Start             = pods.Start
	Stop              = pods.Stop
	Remove            = pods.Remove
	List              = pods.List
)
