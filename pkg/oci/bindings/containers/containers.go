// Package containers re-exports Podman container bindings.
package containers

import "go.podman.io/podman/v6/pkg/bindings/containers"

type (
	StopOptions               = containers.StopOptions
	RemoveOptions             = containers.RemoveOptions
	ListOptions               = containers.ListOptions
	LogOptions                = containers.LogOptions
	WaitOptions               = containers.WaitOptions
	ExecStartAndAttachOptions = containers.ExecStartAndAttachOptions
)

var (
	CreateWithSpec     = containers.CreateWithSpec
	Start              = containers.Start
	Stop               = containers.Stop
	Remove             = containers.Remove
	Inspect            = containers.Inspect
	List               = containers.List
	Logs               = containers.Logs
	Wait               = containers.Wait
	ExecCreate         = containers.ExecCreate
	ExecStartAndAttach = containers.ExecStartAndAttach
	ExecInspect        = containers.ExecInspect
	CopyFromArchive    = containers.CopyFromArchive
	CopyToArchive      = containers.CopyToArchive
)
