package agentlaunch

import (
	"strings"
	"sync"
	"testing"
)

// TestContainmentProbeIsRequestLocal — concurrent FinalizeArgs calls carrying
// different containment probes each get their OWN verdict: a contained request
// keeps its kill-switch, an uncontained one is refused, a contained read-only
// one is still stripped, and an explicit AllowUnsafe still passes uncontained.
// The package-level Containerized is never consulted or swapped, so this runs
// clean under -race and a request's authority cannot leak to its neighbor.
func TestContainmentProbeIsRequestLocal(t *testing.T) {
	t.Setenv(UnsafeLaunchEnv, "")
	prev := Containerized
	Containerized = func() bool { t.Error("package-level Containerized consulted"); return false }
	t.Cleanup(func() { Containerized = prev })

	yes := func() bool { return true }
	no := func() bool { return false }
	const kill = "--dangerously-skip-permissions"
	args := []string{kill, "-p"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			got, err := FinalizeArgs("claude", args, Options{Containerized: yes})
			if err != nil || !containsArg(got, kill) {
				t.Errorf("contained: args=%v err=%v, want kill-switch kept", got, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := FinalizeArgs("claude", args, Options{Containerized: no}); err == nil ||
				!strings.Contains(err.Error(), "refusing") {
				t.Errorf("uncontained: err=%v, want refusal", err)
			}
		}()
		go func() {
			defer wg.Done()
			got, err := FinalizeArgs("claude", args, Options{Containerized: yes, ReadOnly: true})
			if err != nil || containsArg(got, kill) {
				t.Errorf("contained read-only: args=%v err=%v, want kill-switch stripped", got, err)
			}
		}()
		go func() {
			defer wg.Done()
			got, err := FinalizeArgs("claude", args, Options{Containerized: no, AllowUnsafe: true})
			if err != nil || !containsArg(got, kill) {
				t.Errorf("uncontained AllowUnsafe: args=%v err=%v, want kill-switch kept", got, err)
			}
		}()
	}
	wg.Wait()

	if ok, _ := UnsafeLaunchAllowedFor(Options{Containerized: no}); ok {
		t.Error("UnsafeLaunchAllowedFor(uncontained) = true, want false")
	}
	if err := GuardUnsafeArgsFor("claude", args, Options{Containerized: no}); err == nil {
		t.Error("GuardUnsafeArgsFor(uncontained) = nil, want refusal")
	}
}
