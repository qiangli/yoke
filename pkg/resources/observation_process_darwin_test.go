//go:build darwin

package resources

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestHostObservationDarwinNativeCPUAndRSS(t *testing.T) {
	isolateObservation(t)
	if unsafe.Sizeof(observationBSDInfo{}) != 136 || unsafe.Sizeof(observationTaskInfo{}) != 96 {
		t.Fatal("libproc ABI layout mismatch")
	}
	loadObservationLibproc()
	if observationLibproc.err != nil {
		t.Fatal(observationLibproc.err)
	}
	first, err := darwinProcessSample(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if first.RSS == nil || *first.RSS == 0 || first.CPUSeconds == nil || first.Identity.StartID == "" {
		t.Fatalf("native metrics unavailable: %+v", first)
	}
	var before, after syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	deadline := time.Now().Add(30 * time.Millisecond)
	sum := uint64(1)
	for time.Now().Before(deadline) {
		for i := 0; i < 1000; i++ {
			sum = sum*1664525 + 1013904223
		}
	}
	syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	second, err := darwinProcessSample(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	actual := *second.CPUSeconds - *first.CPUSeconds
	reference := float64(after.Utime.Nano()+after.Stime.Nano()-before.Utime.Nano()-before.Stime.Nano()) / 1e9
	if actual < reference*0.5 || actual > reference*2+0.01 {
		t.Fatalf("native CPU units wrong: libproc=%f getrusage=%f sink=%d", actual, reference, sum)
	}
	samples, coverage, err := collectProcessSamples(context.Background())
	if err != nil || len(samples) == 0 || coverage.Included != len(samples) {
		t.Fatalf("native enumeration: count=%d coverage=%+v err=%v", len(samples), coverage, err)
	}
}
