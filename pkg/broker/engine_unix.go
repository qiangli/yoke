//go:build !windows

package broker

import "syscall"

// engineSysProcAttr puts the engine in its own process group so a Ctrl-C at
// the terminal reaches the broker, which then stops the engine in order.
func engineSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// detachSysProcAttr starts `llm up`'s door in its own session, so it
// outlives the shell that asked for it.
func detachSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
