// Package entities re-exports Podman domain entity types.
package entities

import (
	"go.podman.io/podman/v6/pkg/domain/entities"
	entTypes "go.podman.io/podman/v6/pkg/domain/entities/types"
)

type (
	BuildOptions   = entTypes.BuildOptions
	PodSpec        = entTypes.PodSpec
	ServiceOptions = entities.ServiceOptions
)
