package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Moved here with attestEnv when the gate primitive left pkg/herald to break
// the herald <-> acp import cycle. The guarantee is unchanged and belongs with
// the code it constrains.
// TestAttestEnvExcludesSecrets pins that a gate does not inherit the operator's
// vault. A gate is operator-supplied code run to judge a REMOTE party's work;
// handing it every credential in the environment would be a needless blast
// radius.
func TestAttestEnvExcludesSecrets(t *testing.T) {
	t.Setenv("GATE_TEST_FAKE_SECRET", "super-secret-value")
	env := attestEnv("/tmp")
	for _, kv := range env {
		if strings.HasPrefix(kv, "GATE_TEST_FAKE_SECRET=") {
			t.Fatal("gate environment must not carry arbitrary parent env vars")
		}
	}
	var sawPath bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Error("gate environment should preserve PATH so build/test gates work")
	}
}

// SILENCE IS RECORDED, NOT PUNISHED.
//
// An earlier draft of this package flipped Passed to false whenever a gate
// exited 0 with no output. That breaks the Unix convention it was trying to
// police: `go build ./...`, `go vet` and `test -f x` all say nothing when they
// succeed, and THIS PROJECT'S OWN GATE is `go vet ... && go test ...`. Under
// that rule a clean vet abstained, and every quiet gate in the tree stopped
// passing.
//
// The signal is still worth keeping — a gate that is ALWAYS silent is a
// vacuity candidate — so it is reported as an observation the caller can act
// on, while the verdict stays the command's own.
func TestRunLocalSilentSuccessPassesAndIsFlagged(t *testing.T) {
	requirePOSIXShell(t)
	o := RunLocal(context.Background(), t.TempDir(), "exit 0", "completed")
	if !o.Ran {
		t.Fatalf("gate did not run: %+v", o)
	}
	if !o.Passed || !o.Trusted() {
		t.Errorf("a silent exit 0 is a PASS — flipping it abstains on a clean `go vet`: %+v", o)
	}
	if !o.Silent {
		t.Errorf("silence was not recorded; the caller cannot see it: %+v", o)
	}
	if o.Abstained {
		t.Errorf("Abstained is for an unproved PROBE, not for silence: %+v", o)
	}
}

// A gate that speaks keeps Silent false, so the flag distinguishes the two
// rather than being permanently on.
func TestRunLocalNoisySuccessIsNotSilent(t *testing.T) {
	requirePOSIXShell(t)
	o := RunLocal(context.Background(), t.TempDir(), "echo evidence", "completed")
	if !o.Passed || o.Silent {
		t.Fatalf("a gate that produced output must not be flagged Silent: %+v", o)
	}
}

func TestRunLocalMutationProbe(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "MUTATED")
	probe := &MutationProbe{
		Name: "marker appears",
		Mutate: func() (func() error, error) {
			if err := os.WriteFile(marker, []byte("mutation"), 0o644); err != nil {
				return nil, err
			}
			return func() error { return os.Remove(marker) }, nil
		},
	}
	command := "if test -f MUTATED; then echo mutation; exit 2; else echo healthy; fi"
	o := RunLocalWithProbe(context.Background(), dir, command, "completed", probe)
	if !o.Passed || !o.Trusted() || !o.Probe.Proved {
		t.Fatalf("mutation-proved local gate did not pass: %+v", o)
	}
}
