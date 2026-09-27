//go:build windows

package broker

import "syscall"

func engineSysProcAttr() *syscall.SysProcAttr { return nil }

func detachSysProcAttr() *syscall.SysProcAttr { return nil }
