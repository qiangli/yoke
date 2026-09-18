//go:build darwin

package resources

import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Public ABI: Apple SDK libproc.h and sys/proc_info.h (PROC_PIDTASKALLINFO).
// XNU bsd/kern/proc_info.c delegates to fill_taskprocinfo; osfmk/kern/
// bsd_kern.c fills CPU from recount_times_mach. Those are Mach absolute ticks,
// converted through mach_timebase_info, while resident size is already bytes.
// References: https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info.h
// https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/kern/bsd_kern.c
// ABI declarations only; no Apple implementation source is incorporated.
type observationBSDInfo struct {
	Fields                    [12]uint32
	Command                   [16]byte
	Name                      [32]byte
	Tail                      [6]uint32
	StartSeconds, StartMicros uint64
}
type observationTaskInfo struct {
	Virtual, RSS, User, System, ThreadsUser, ThreadsSystem uint64
	Fields                                                 [12]int32
}
type observationTaskAll struct {
	BSD  observationBSDInfo
	Task observationTaskInfo
}

var observationLibproc struct {
	once         sync.Once
	err          error
	list         func(uint32, uint32, unsafe.Pointer, int32) int32
	info         func(int32, int32, uint64, unsafe.Pointer, int32) int32
	nanosPerTick float64
}

func loadObservationLibproc() {
	observationLibproc.once.Do(func() {
		h, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			observationLibproc.err = err
			return
		}
		bind := func(name string, target any) bool {
			address, err := purego.Dlsym(h, name)
			if err != nil {
				observationLibproc.err = err
				return false
			}
			purego.RegisterFunc(target, address)
			return true
		}
		if !bind("proc_listpids", &observationLibproc.list) || !bind("proc_pidinfo", &observationLibproc.info) {
			return
		}
		mach, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			observationLibproc.err = err
			return
		}
		address, err := purego.Dlsym(mach, "mach_timebase_info")
		if err != nil {
			observationLibproc.err = err
			return
		}
		var timebase func(unsafe.Pointer) int32
		purego.RegisterFunc(&timebase, address)
		var ratio [2]uint32
		if timebase(unsafe.Pointer(&ratio)) != 0 || ratio[1] == 0 {
			observationLibproc.err = fmt.Errorf("mach timebase unavailable")
			return
		}
		observationLibproc.nanosPerTick = float64(ratio[0]) / float64(ratio[1])
	})
}
func collectProcessSamples(ctx context.Context) ([]processSample, ObservationCoverage, error) {
	loadObservationLibproc()
	coverage := ObservationCoverage{Complete: true, Limit: maxObservedProcesses}
	if observationLibproc.err != nil {
		return nil, coverage, observationLibproc.err
	}
	ctx, cancel := processBudget(ctx)
	defer cancel()
	pids := make([]int32, maxObservedProcesses+1)
	n := observationLibproc.list(1, 0, unsafe.Pointer(&pids[0]), int32(len(pids)*4))
	if n <= 0 {
		return nil, coverage, fmt.Errorf("proc_listpids unavailable")
	}
	count := min(int(n)/4, len(pids))
	coverage.Seen = count
	if count > maxObservedProcesses {
		coverage.Complete = false
		coverage.Reason = "process count limit"
		count = maxObservedProcesses
	}
	out := make([]processSample, 0, count)
	for _, pid := range pids[:count] {
		if err := ctx.Err(); err != nil {
			coverage.Complete = false
			coverage.Reason = err.Error()
			break
		}
		if pid <= 0 {
			continue
		}
		p, err := darwinProcessSample(pid)
		if err != nil {
			coverage.Complete = false
			coverage.Reason = "process exited or access denied"
			continue
		}
		out = append(out, p)
	}
	coverage.Included = len(out)
	return out, coverage, nil
}
func darwinProcessSample(pid int32) (processSample, error) {
	var info observationTaskAll
	n := observationLibproc.info(pid, 2, 0, unsafe.Pointer(&info), int32(unsafe.Sizeof(info)))
	if n != int32(unsafe.Sizeof(info)) || int32(info.BSD.Fields[3]) != pid {
		return processSample{}, fmt.Errorf("proc_pidinfo unavailable for pid %d", pid)
	}
	name := info.BSD.Name[:]
	if name[0] == 0 {
		name = info.BSD.Command[:]
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	cpu := (float64(info.Task.User) + float64(info.Task.System)) * observationLibproc.nanosPerTick / 1e9
	return processSample{Identity: ProcessIdentity{PID: int(pid), StartID: fmt.Sprintf("darwin:%d:%d", info.BSD.StartSeconds, info.BSD.StartMicros)}, PPID: int(info.BSD.Fields[4]), Name: string(name[:end]), CPUSeconds: &cpu, RSS: &info.Task.RSS, Source: "libproc:taskallinfo"}, nil
}

func lookupNativeProcessIdentity(pid int) (ProcessIdentity, error) {
	loadObservationLibproc()
	if observationLibproc.err != nil {
		return ProcessIdentity{}, observationLibproc.err
	}
	sample, err := darwinProcessSample(int32(pid))
	return sample.Identity, err
}
