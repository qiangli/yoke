// Package specgen re-exports Podman spec generation types.
package specgen

import "go.podman.io/podman/v6/pkg/specgen"

type (
	SpecGenerator    = specgen.SpecGenerator
	PodSpecGenerator = specgen.PodSpecGenerator
	PodBasicConfig   = specgen.PodBasicConfig
	Namespace        = specgen.Namespace
	NamespaceMode    = specgen.NamespaceMode
)

var NewSpecGenerator = specgen.NewSpecGenerator

// Namespace mode constants re-exported so callers can build
// SpecGenerator.{Cgroup,IPC,Net,PID,User,UTS}NS values without
// importing the upstream specgen path directly. Mirrors what
// upstream podman CLI accepts for --cgroupns / --ipc / --net etc.
const (
	Host    = specgen.Host
	Private = specgen.Private
	None    = specgen.None
	Default = specgen.Default
)
