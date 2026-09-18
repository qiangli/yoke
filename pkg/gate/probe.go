package gate

import (
	"context"
	"fmt"
)

// MutationProbe describes one reversible, structure-preserving perturbation.
// Mutate must apply the perturbation and return a restoration function.
//
// Probing is deliberately opt-in per run: a project-specific mutation cannot
// be inferred safely, and running a full gate twice on every developer command
// is too expensive to keep enabled. ProbeResult.Ran makes that choice visible.
// The honest limitation is that an unprobed gate which emits non-empty but
// semantically vacuous output can still pass.
type MutationProbe struct {
	Name   string
	Mutate func() (restore func() error, err error)
}

// ProbeResult is deliberately present even when no probe ran. Consumers can
// distinguish "proved capable of failing" from "not asked".
type ProbeResult struct {
	Ran              bool   `json:"ran"`
	Name             string `json:"name,omitempty"`
	MutationDetected bool   `json:"mutation_detected"`
	Restored         bool   `json:"restored"`
	RestoredPassed   bool   `json:"restored_passed"`
	Proved           bool   `json:"proved"`
	Error            string `json:"error,omitempty"`
}

// ProveGuardMutation mutation-tests a guard: the guard must fail with the
// perturbation applied and pass after restoration. This is the reusable form of
// "break it, confirm red; restore it, confirm green".
//
// check returns true when the guard passes. A false result is a measured guard
// failure; an error means the guard itself could not be evaluated.
func ProveGuardMutation(ctx context.Context, probe MutationProbe, check func(context.Context) (bool, error)) ProbeResult {
	r := ProbeResult{Ran: true, Name: probe.Name}
	if probe.Mutate == nil {
		r.Error = "gate: mutation probe has no mutation"
		return r
	}
	if check == nil {
		r.Error = "gate: mutation probe has no guard"
		return r
	}

	restore, err := probe.Mutate()
	if err != nil {
		r.Error = fmt.Sprintf("gate: apply mutation: %v", err)
		return r
	}
	if restore == nil {
		r.Error = "gate: mutation probe returned no restoration"
		return r
	}
	restoreAttempted := false
	defer func() {
		// Best effort for a guard that panics. Normal returns restore below so
		// that the post-restoration green can be measured.
		if !restoreAttempted {
			_ = restore()
		}
	}()

	mutatedPassed, checkErr := check(ctx)
	r.MutationDetected = checkErr == nil && !mutatedPassed

	restoreAttempted = true
	restoreErr := restore()
	r.Restored = restoreErr == nil
	if restoreErr == nil {
		r.RestoredPassed, err = check(ctx)
		if err != nil {
			checkErr = err
		}
	}

	switch {
	case restoreErr != nil:
		r.Error = fmt.Sprintf("gate: restore mutation: %v", restoreErr)
	case checkErr != nil:
		r.Error = fmt.Sprintf("gate: run mutation probe: %v", checkErr)
	case !r.MutationDetected:
		r.Error = "gate: mutation was not detected; the guard stayed green"
	case !r.RestoredPassed:
		r.Error = "gate: guard did not return green after restoration"
	default:
		r.Proved = true
	}
	return r
}
