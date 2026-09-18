// Package spec re-exports OCI runtime spec types.
package spec

import specs "github.com/opencontainers/runtime-spec/specs-go"

type (
	Mount          = specs.Mount
	LinuxResources = specs.LinuxResources
)
