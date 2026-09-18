// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func contractEngine(t *testing.T, dir, md string) *Engine {
	t.Helper()
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
}

func TestContractEnsurePasses(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### make\n" +
		"Generates: out.txt\nEnsure: file-exists out.txt\nEffects: write\n" +
		block("bash", "echo hi > out.txt")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "make")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusDone {
		t.Fatalf("want done, got %s (%v)", r.Status, r.Err)
	}
	if r.Attestation == nil || !r.Attestation.Valid {
		t.Fatalf("attestation not valid: %+v", r.Attestation)
	}
	if len(r.Attestation.Effects) != 1 || r.Attestation.Effects[0] != "write" {
		t.Errorf("effects = %v", r.Attestation.Effects)
	}
}

func TestContractEnsureFailsTargetDespiteCleanExit(t *testing.T) {
	dir := t.TempDir()
	// Body exits 0 but does NOT create the promised output → postcondition fails.
	md := "## Tasks\n\n### lie\n" +
		"Ensure: file-exists out.txt\n" +
		block("bash", "echo 'I did nothing'")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "lie")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusFailed {
		t.Fatalf("clean exit + failed postcondition should fail the target; got %s", r.Status)
	}
	if r.ExitCode != 3 { // weavecli.ExitPrecondFail
		t.Errorf("want exit 3 (precond), got %d", r.ExitCode)
	}
	if r.Attestation == nil || r.Attestation.Valid {
		t.Errorf("attestation should be invalid: %+v", r.Attestation)
	}
}

func TestContractEnsureRawShellCommand(t *testing.T) {
	dir := t.TempDir()
	// A bare shell command as the postcondition: must exit 0.
	md := "## Tasks\n\n### t\n" +
		"Ensure: test \"$(cat marker)\" = ok\n" +
		block("bash", "echo ok > marker")
	report, _ := contractEngine(t, dir, md).Run(context.Background(), "t")
	if report.Results[0].Status != StatusDone {
		t.Errorf("raw-command postcondition should pass; got %s (%v)",
			report.Results[0].Status, report.Results[0].Err)
	}
}

func TestContractUnknownEffectRejected(t *testing.T) {
	md := "## Tasks\n\n### t\nEffects: teleport\n" + block("bash", "true")
	if _, err := BuildGraph(doc(t, md)); err == nil {
		t.Fatal("want error for unknown effect")
	} else if ExitCodeOf(err) != 2 {
		t.Errorf("want exit 2, got %d", ExitCodeOf(err))
	}
}
