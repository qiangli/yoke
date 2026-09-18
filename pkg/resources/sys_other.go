//go:build !linux && !darwin && !windows

package resources

import (
	"errors"
	"runtime"
	"time"
)

// The three shipped platforms are linux, darwin, and windows. Anywhere
// else the collector still runs and still returns an envelope — every
// subsystem reports "unsupported on <goos>" through Warnings rather than
// inventing a number.

var errUnsupported = errors.New("system resource probes are not implemented on " + runtime.GOOS)

func sampleCounters() (counters, error) {
	return counters{at: time.Now(), unsampled: true}, errUnsupported
}

func cpuStatic() (string, int, int)  { return "", runtime.NumCPU(), 0 }
func loadAverage() ([]float64, bool) { return nil, false }
func memoryStats() (Memory, error)   { return Memory{}, errUnsupported }
func diskStats() ([]Disk, error)     { return nil, errUnsupported }
func gpuStats() ([]GPU, error)       { return nil, errUnsupported }
