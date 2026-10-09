//go:build windows

package broker

import (
	"syscall"

	"golang.org/x/sys/windows"
)

func engineSysProcAttr() *syscall.SysProcAttr {
	// The engine runs in a job object that dies with its last handle: the
	// door holds the only one (engineJobHandle), so closing it in
	// stopEngineProcess stops the engine and every runner it spawned.
	// CREATE_NEW_PROCESS_GROUP keeps the engine out of the door's Ctrl-C
	// group, as Setpgid does on Unix.
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func detachSysProcAttr() *syscall.SysProcAttr { return nil }
