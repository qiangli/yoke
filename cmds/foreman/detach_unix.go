//go:build unix

package foremancmd

import "syscall"

func foremanDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
