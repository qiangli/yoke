// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build darwin

package artifact

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

type procBSDInfo struct {
	Fields                    [12]uint32
	Command                   [16]byte
	Name                      [32]byte
	Tail                      [6]uint32
	StartSeconds, StartMicros uint64
}

type procTaskInfo struct {
	Virtual, RSS, User, System, ThreadsUser, ThreadsSystem uint64
	Fields                                                 [12]int32
}

type procTaskAll struct {
	BSD  procBSDInfo
	Task procTaskInfo
}

var darwinLibproc struct {
	once sync.Once
	err  error
	info func(int32, int32, uint64, unsafe.Pointer, int32) int32
}

func loadDarwinLibproc() {
	darwinLibproc.once.Do(func() {
		h, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			darwinLibproc.err = err
			return
		}
		addr, err := purego.Dlsym(h, "proc_pidinfo")
		if err != nil {
			darwinLibproc.err = err
			return
		}
		purego.RegisterFunc(&darwinLibproc.info, addr)
	})
}

func lookupProcessStartID(pid int) (string, error) {
	if pid <= 0 || pid > 1<<31-1 {
		return "", os.ErrInvalid
	}
	loadDarwinLibproc()
	if darwinLibproc.err != nil {
		return "", darwinLibproc.err
	}
	var info procTaskAll
	n := darwinLibproc.info(int32(pid), 2, 0, unsafe.Pointer(&info), int32(unsafe.Sizeof(info)))
	if n != int32(unsafe.Sizeof(info)) || int32(info.BSD.Fields[3]) != int32(pid) {
		if !processExists(pid) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("proc_pidinfo unavailable for pid %d", pid)
	}
	return fmt.Sprintf("darwin:%d:%d", info.BSD.StartSeconds, info.BSD.StartMicros), nil
}
