//go:build linux

package resources

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func collectProcessSamples(ctx context.Context) ([]processSample, ObservationCoverage, error) {
	ctx, cancel := processBudget(ctx)
	defer cancel()
	coverage := ObservationCoverage{Complete: true, Limit: maxObservedProcesses}
	f, err := os.Open("/proc")
	if err != nil {
		return nil, coverage, err
	}
	defer f.Close()
	boot, _ := readSmallObservationFile("/proc/sys/kernel/random/boot_id", 128)
	hz := observationClockTicks()
	var out []processSample
	for {
		names, err := f.Readdirnames(128)
		for _, name := range names {
			pid, err := strconv.Atoi(name)
			if err != nil {
				continue
			}
			coverage.Seen++
			if ctx.Err() != nil || len(out) >= maxObservedProcesses {
				coverage.Complete = false
				coverage.Reason = "process count or time limit"
				coverage.Included = len(out)
				return out, coverage, nil
			}
			b, err := readSmallObservationFile(filepath.Join("/proc", name, "stat"), 16384)
			if err != nil {
				coverage.Complete = false
				coverage.Reason = "process exited or access denied"
				continue
			}
			p, err := parseObservationProcStat(pid, b, strings.TrimSpace(string(boot)), hz, uint64(os.Getpagesize()))
			if err != nil {
				coverage.Complete = false
				coverage.Reason = "unreadable process stat"
				continue
			}
			out = append(out, p)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, coverage, err
		}
	}
	coverage.Included = len(out)
	return out, coverage, nil
}
func readSmallObservationFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("process metadata exceeds bound")
	}
	return b, err
}
func observationClockTicks() float64 {
	b, err := readSmallObservationFile("/proc/self/auxv", 8192)
	if err != nil {
		return 0
	}
	word := strconv.IntSize / 8
	for i := 0; i+word*2 <= len(b); i += word * 2 {
		var key, value uint64
		if word == 8 {
			key = binary.NativeEndian.Uint64(b[i:])
			value = binary.NativeEndian.Uint64(b[i+word:])
		} else {
			key = uint64(binary.NativeEndian.Uint32(b[i:]))
			value = uint64(binary.NativeEndian.Uint32(b[i+word:]))
		}
		if key == 17 && value > 0 {
			return float64(value)
		}
	}
	return 0
}

func lookupNativeProcessIdentity(pid int) (ProcessIdentity, error) {
	boot, err := readSmallObservationFile("/proc/sys/kernel/random/boot_id", 128)
	if err != nil {
		return ProcessIdentity{}, err
	}
	stat, err := readSmallObservationFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"), 16384)
	if err != nil {
		return ProcessIdentity{}, err
	}
	sample, err := parseObservationProcStat(pid, stat, strings.TrimSpace(string(boot)), 0, uint64(os.Getpagesize()))
	return sample.Identity, err
}
