// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sprint 203 — Require: is the precondition pair of Ensure:. Before this
// story the key was silently dropped and the body ran regardless.

func TestContractRequireFalseSkipsBody(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### guarded\n" +
		"Require: false\n" +
		block("bash", "echo ran > out.txt")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "guarded")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusFailed {
		t.Fatalf("failed precondition should fail the target; got %s", r.Status)
	}
	if r.ExitCode != 3 { // weavecli.ExitPrecondFail
		t.Errorf("want exit 3 (precond), got %d", r.ExitCode)
	}
	if r.Err == nil || !strings.HasPrefix(r.Err.Error(), "precondition failed: false") {
		t.Errorf("error must name the clause and the check, got %v", r.Err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out.txt")); statErr == nil {
		t.Fatal("body ran despite a failed precondition")
	}
	if r.Attestation == nil || r.Attestation.Valid || r.Attestation.Clause != "require" {
		t.Errorf("attestation should be an invalid require verdict: %+v", r.Attestation)
	}
	if len(r.Attestation.Checks) != 1 || r.Attestation.Checks[0].Expr != "false" || r.Attestation.Checks[0].Pass {
		t.Errorf("checks = %+v", r.Attestation.Checks)
	}
}

func TestContractRequireTrueThenEnsureFails(t *testing.T) {
	dir := t.TempDir()
	// Precondition holds, body runs and exits 0, postcondition fails: the
	// message must name the POSTcondition, not the precondition.
	md := "## Tasks\n\n### lie\n" +
		"Require: true\nEnsure: file-exists out.txt\n" +
		block("bash", "echo ran > ran.txt")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "lie")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusFailed || r.ExitCode != 3 {
		t.Fatalf("want failed/3, got %s/%d", r.Status, r.ExitCode)
	}
	if r.Err == nil || !strings.HasPrefix(r.Err.Error(), "postcondition failed: file-exists out.txt") {
		t.Errorf("error must name the postcondition, got %v", r.Err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran.txt")); statErr != nil {
		t.Fatal("body did not run although the precondition held")
	}
	if r.Attestation == nil || r.Attestation.Valid || r.Attestation.Clause != "" {
		t.Errorf("attestation should be the postcondition verdict: %+v", r.Attestation)
	}
}

func TestContractRequireHoldsBodyRuns(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### ok\n" +
		"Require: test -d .\nRequire: file-absent out.txt\nEnsure: file-exists out.txt\n" +
		block("bash", "echo hi > out.txt")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusDone {
		t.Fatalf("want done, got %s (%v)", r.Status, r.Err)
	}
	if r.Attestation == nil || !r.Attestation.Valid || r.Attestation.Clause != "" {
		t.Errorf("attestation = %+v", r.Attestation)
	}
}

func TestContractRequireNotRetried(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### guarded\n" +
		"Require: false\nRetries: 2\n" +
		block("bash", "echo ran >> out.txt")
	report, err := contractEngine(t, dir, md).Run(context.Background(), "guarded")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := report.Results[0].Status; got != StatusFailed {
		t.Fatalf("want failed, got %s", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out.txt")); statErr == nil {
		t.Fatal("body ran on a retry despite a failed precondition")
	}
}

func TestContractRequireParsedAndClassified(t *testing.T) {
	d := doc(t, "## Tasks\n\n### t\nRequire: test 1 = 1\nRequires: dep\nEnsure: true\n"+block("bash", ":")+"\n### dep\n"+block("bash", ":"))
	var target *Task
	for _, x := range d.Tasks {
		if x.Name == "t" {
			target = x
		}
	}
	if target == nil {
		t.Fatal("target t not parsed")
	}
	if len(target.Require) != 1 || target.Require[0] != "test 1 = 1" {
		t.Errorf("Require = %v", target.Require)
	}
	if len(target.Requires) != 1 || target.Requires[0] != "dep" {
		t.Errorf("Requires (make prerequisites) = %v; Require: must not be confused with it", target.Requires)
	}
	// A precondition failure classifies as its own stable code, never as
	// "postcondition-failed".
	rec := RecordAttempt(target, nil, 1, TaskResult{Name: "t", Status: StatusFailed, ExitCode: 3,
		Attestation: &Attestation{Target: "t", Clause: "require", Checks: []CheckResult{{Expr: "false"}}}})
	if rec.Failure == nil || rec.Failure.Code != FailPrecondition {
		t.Errorf("failure = %+v, want %s", rec.Failure, FailPrecondition)
	}
}
