//go:build !windows

package room

// The same-process identity half of the liveness rule: a holder pid is only
// the holder while the process that STARTED at the recorded time still wears
// it. A recycled pid is a stranger, not a claim.

import (
	"os"
	"testing"
)

// The identity proof must be a fingerprint, not a coin flip: reading the
// same live process twice must agree, and two processes must not share one.
func TestPidStartIsStableAndDistinct(t *testing.T) {
	self, ok := pidStart(os.Getpid())
	if !ok {
		t.Skip("this OS cannot report a process start time")
	}
	again, _ := pidStart(os.Getpid())
	if self != again {
		t.Fatalf("start time of one live process moved: %q -> %q", self, again)
	}
	other, ok := pidStart(liveChildPID(t))
	if ok && other == self {
		t.Fatalf("two processes share one start time %q — it cannot prove identity", self)
	}
}

// sanity for the helper itself, so a seeding bug cannot masquerade as a
// liveness verdict: a gone pid has no start time.
func TestPidStartOfAGonePid(t *testing.T) {
	if s, ok := pidStart(deadPID(t)); ok {
		t.Fatalf("reaped pid still reports start time %q", s)
	}
}
