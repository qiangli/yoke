// Package machine re-exports Podman machine management types and functions.
package machine

import (
	"go.podman.io/podman/v6/pkg/machine"
	machineDefine "go.podman.io/podman/v6/pkg/machine/define"
	"go.podman.io/podman/v6/pkg/machine/env"
	"go.podman.io/podman/v6/pkg/machine/provider"
	"go.podman.io/podman/v6/pkg/machine/shim"
	"go.podman.io/podman/v6/pkg/machine/vmconfigs"
)

type (
	StartOptions  = machine.StartOptions
	InitOptions   = machineDefine.InitOptions
	RemoveOptions = machine.RemoveOptions
	ResetOptions  = machine.ResetOptions
	ListOptions   = machine.ListOptions
	ListResponse  = machine.ListResponse
	Status        = machineDefine.Status
	MachineConfig = vmconfigs.MachineConfig
	VMProvider    = vmconfigs.VMProvider
)

const Running = machineDefine.Running

var (
	GetProvider       = provider.Get
	Init              = shim.Init
	Start             = shim.Start
	Stop              = shim.Stop
	Remove            = shim.Remove
	Reset             = shim.Reset
	List              = shim.List
	GetMachineDirs    = env.GetMachineDirs
	LoadMachineByName = vmconfigs.LoadMachineByName
)
