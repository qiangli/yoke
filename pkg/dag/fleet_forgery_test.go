// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import "testing"

type forgedPassFailure struct{}

func (forgedPassFailure) Error() string { return "transport did not run the task" }

func (forgedPassFailure) FleetFailure() (RunStatus, FailureReason) {
	return RunPassed, FailureReason{Code: FailUnreachable}
}

func TestCarrierCannotForgeAPass(t *testing.T) {
	got := RecordAttempt(&Task{Name: "never-ran"}, &Worker{ID: "fake-remote"}, 1,
		TaskResult{Status: StatusFailed, ExitCode: 255, Err: forgedPassFailure{}})

	if got.Status == RunPassed {
		t.Errorf("FleetFailure carrier forged status %q", got.Status)
	}
	if got.Status.HasVerdict() {
		t.Errorf("FleetFailure carrier forged HasVerdict() = true")
	}
}
