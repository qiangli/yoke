package kb

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func workflowFixture() WorkflowLearning {
	return WorkflowLearning{Workflow: "todo:cbb6026e8cdf", Candidate: "yoke:add54059", Finding: "preserve-effect-scope", Kind: "skill", Target: "handoff", Applicability: "Recovery with declared portable effects; unknown effects require reconciliation", Reports: []WorkflowReport{{Reporter: "worker", Episode: "recovery-1", Ref: "gate:handoff-package", Outcome: "passed", Summary: "Focused package gate passed; live recovery not yet verified"}}}
}
func importWorkflow(t *testing.T, dir string, w WorkflowLearning, apply bool) (string, error) {
	t.Helper()
	result, err := ProposeWorkflow(dir, "repo", w, apply, func(w WorkflowLearning) error { return nil })
	b, _ := json.Marshal(result)
	return string(b), err
}
func TestWorkflowCandidateDedupAndProvenance(t *testing.T) {
	dir := t.TempDir()
	w := workflowFixture()
	slug := workflowKey(w)
	if _, err := importWorkflow(t, dir, w, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir).Load(slug); !os.IsNotExist(err) {
		t.Fatalf("preview wrote a page: %v", err)
	}
	if _, err := importWorkflow(t, dir, w, true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(Open(dir).PagePath(slug))
	out, err := importWorkflow(t, dir, w, true)
	if err != nil || !strings.Contains(out, `"duplicate":true`) {
		t.Fatalf("retry: %s %v", out, err)
	}
	after, _ := os.ReadFile(Open(dir).PagePath(slug))
	if string(before) != string(after) {
		t.Fatal("retry rewrote candidate")
	}
	w.Reports[0].Reporter = "reviewer"
	w.Reports[0].Ref = "todo:705359dbde34"
	w.Reports[0].Outcome = "passed"
	if _, err := importWorkflow(t, dir, w, true); err != nil {
		t.Fatal(err)
	}
	p, err := Open(dir).Load(slug)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusCandidate || p.Evidence != "" || p.Workflow == nil || len(p.Workflow.Reports) != 2 || p.Description != w.Applicability {
		t.Fatalf("lost candidate/provenance: %+v", p)
	}
	if !strings.Contains(p.Body, "todo:cdae01f261b1") || !strings.Contains(p.Body, "skill learn/run") {
		t.Fatal("existing channels missing")
	}
	pages, _ := Open(dir).List()
	if len(pages) != 1 {
		t.Fatalf("reports duplicated pages: %d", len(pages))
	}
	// Existing validation is explicit and cannot be undone by reimport.
	mustRun(t, dir, "validate", slug, "--evidence", "review:exact-candidate")
	w.Reports[0].Summary = "new evidence"
	if _, err := importWorkflow(t, dir, w, true); err == nil {
		t.Fatal("changed validated record")
	}
	p, _ = Open(dir).Load(slug)
	if p.Status != StatusValidated || len(p.Workflow.Reports) != 2 {
		t.Fatal("validated record changed")
	}
}
func TestWorkflowConcurrentReports(t *testing.T) {
	dir := t.TempDir()
	w := workflowFixture()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := workflowFixture()
			v.Reports[0].Reporter = string(rune('a' + i))
			_, err := importWorkflow(t, dir, v, true)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := Open(dir).Load(workflowKey(w))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Workflow.Reports) != 8 {
		t.Fatalf("lost concurrent provenance: %d", len(p.Workflow.Reports))
	}
}
func TestWorkflowRejectsMalformedAndKeepsOutcomesCandidate(t *testing.T) {
	for _, outcome := range []string{"passed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			w := workflowFixture()
			w.Kind = "incident"
			w.Reports[0].Outcome = outcome
			if _, err := importWorkflow(t, dir, w, true); err != nil {
				t.Fatal(err)
			}
			p, _ := Open(dir).Load(workflowKey(w))
			if p.Status != StatusCandidate || !strings.Contains(p.Body, "shape incident") {
				t.Fatal("incident bypassed boundary")
			}
		})
	}
	for _, change := range []func(*WorkflowLearning){func(w *WorkflowLearning) { w.Reports = nil }, func(w *WorkflowLearning) { w.Candidate = "" }, func(w *WorkflowLearning) { w.Applicability = "" }, func(w *WorkflowLearning) { w.Reports[0].Ref = "" }, func(w *WorkflowLearning) { w.Reports[0].Outcome = "verified" }} {
		w := workflowFixture()
		change(&w)
		if _, err := importWorkflow(t, t.TempDir(), w, true); err == nil {
			t.Fatal("invalid report accepted")
		}
	}
	w := workflowFixture()
	b, _ := json.Marshal(w)
	for _, input := range []string{string(b) + ` {}`, strings.Replace(string(b), `"reports":`, `"validated":true,"reports":`, 1)} {
		if _, err := run(t, t.TempDir(), input, "workflow", "--apply"); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	// Candidate, target and applicability are all part of dedup identity.
	original := workflowKey(w)
	w.Candidate = "yoke:other"
	if workflowKey(w) == original {
		t.Fatal("candidate identity lost")
	}
}

func TestWorkflowPrivacyAndSizeBeforeWrite(t *testing.T) {
	old := shareableWriteCheck
	shareableWriteCheck = func(s string) error {
		if strings.Contains(s, "private-evidence") {
			return fmt.Errorf("private reference")
		}
		return nil
	}
	t.Cleanup(func() { shareableWriteCheck = old })
	dir := t.TempDir()
	w := workflowFixture()
	w.Reports[0].Ref = "private-evidence"
	if _, err := importWorkflow(t, dir, w, true); err == nil {
		t.Fatal("provenance bypassed privacy check")
	}
	pages, err := Open(dir).List()
	if err != nil || len(pages) != 0 {
		t.Fatalf("refusal mutated store: %v %v", pages, err)
	}
	w = workflowFixture()
	b, _ := json.Marshal(w)
	if _, err := run(t, dir, string(b)+strings.Repeat(" ", 1<<20), "workflow", "--apply"); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestWorkflowRequiresIndependentVerification(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "unverified", "cancelled"} {
		for _, resolver := range []string{"missing", "reject", "accept"} {
			t.Run(outcome+"/"+resolver, func(t *testing.T) {
				dir := t.TempDir()
				w := workflowFixture()
				w.Reports[0].Outcome = outcome
				var verify WorkflowVerifier
				if resolver == "reject" {
					verify = func(WorkflowLearning) error { return fmt.Errorf("candidate does not match trusted workflow gate") }
				}
				if resolver == "accept" {
					verify = func(got WorkflowLearning) error {
						if got.Candidate != w.Candidate || got.Workflow != w.Workflow || got.Applicability != w.Applicability || got.Reports[0].Ref != w.Reports[0].Ref {
							t.Fatal("resolver lost evidence scope")
						}
						return nil
					}
				}
				_, err := ProposeWorkflow(dir, "repo", w, true, verify)
				allowed := resolver == "accept" && (outcome == "passed" || outcome == "failed")
				if (err == nil) != allowed {
					t.Fatalf("allowed=%v err=%v", allowed, err)
				}
				pages, _ := Open(dir).List()
				if !allowed && len(pages) != 0 {
					t.Fatal("refused evidence wrote knowledge")
				}
			})
		}
	}
}

func TestWorkflowWrittenPageOmitsHumanLogin(t *testing.T) {
	for _, ring := range []string{"repo", "host", "agent", "user", "dir"} {
		t.Run(ring, func(t *testing.T) {
			for _, key := range []string{"WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_AGENT", "CLAUDECODE"} {
				t.Setenv(key, "")
			}
			t.Setenv("USER", "synthetic-private-login")
			t.Setenv("LOGNAME", "synthetic-private-login")
			dir := t.TempDir()
			w := workflowFixture()
			result, err := ImportWorkflowReport(dir, ring, w, true)
			if err != nil {
				t.Fatal(err)
			}
			page, err := os.ReadFile(Open(dir).PagePath(result.Slug))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(page), "synthetic-private-login") {
				t.Fatal("written page leaked human login")
			}
			p, err := Open(dir).Load(result.Slug)
			if err != nil || p.Source.Tool != "workflow-proposer" || p.Status != StatusCandidate {
				t.Fatalf("unexpected provenance: %+v %v", p, err)
			}
		})
	}
}

func TestWorkflowResolverBindsCompletedFixture(t *testing.T) {
	trusted := workflowFixture()
	trusted.Reports[0].Summary = "Synthetic completed recovery fixture"
	resolver := func(w WorkflowLearning) error {
		if w.Workflow != trusted.Workflow || w.Candidate != trusted.Candidate || w.Applicability != trusted.Applicability {
			return fmt.Errorf("workflow scope mismatch")
		}
		for _, r := range w.Reports {
			if r.Ref != trusted.Reports[0].Ref || r.Outcome != trusted.Reports[0].Outcome {
				return fmt.Errorf("report not established by completed fixture")
			}
		}
		return nil
	}
	for _, field := range []string{"unchanged", "workflow", "candidate", "applicability", "ref", "outcome"} {
		t.Run(field, func(t *testing.T) {
			w := workflowFixture()
			switch field {
			case "workflow":
				w.Workflow = "different-workflow"
			case "candidate":
				w.Candidate = "different-candidate"
			case "applicability":
				w.Applicability = "different-scope"
			case "ref":
				w.Reports[0].Ref = "unknown-gate"
			case "outcome":
				w.Reports[0].Outcome = "failed"
			}
			dir := t.TempDir()
			_, err := ProposeWorkflow(dir, "repo", w, true, resolver)
			if (err == nil) != (field == "unchanged") {
				t.Fatalf("unexpected result: %v", err)
			}
			pages, _ := Open(dir).List()
			if field != "unchanged" && len(pages) != 0 {
				t.Fatal("unverified evidence persisted")
			}
		})
	}
}

// Exercise the production command: reported success is durable, never validation.
func TestWorkflowCLIReportedEvidence(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "unverified", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			w := workflowFixture()
			w.Workflow = "todo:705359dbde34"
			w.Reports[0].Outcome = outcome
			w.Reports[0].Summary = "Synthetic S10 source report; supplied outcome is not an attestation"
			b, _ := json.Marshal(w)
			if _, err := run(t, dir, string(b), "workflow"); err != nil {
				t.Fatal(err)
			}
			pages, _ := Open(dir).List()
			if len(pages) != 0 {
				t.Fatal("preview wrote")
			}
			for i := 0; i < 2; i++ {
				out, err := run(t, dir, string(b), "workflow", "--apply")
				if err != nil {
					t.Fatal(err)
				}
				var result WorkflowProposal
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if !result.Applied || result.Status != StatusCandidate || result.Duplicate != (i == 1) {
					t.Fatalf("dishonest result: %+v", result)
				}
			}
			p, err := Open(dir).Load(workflowKey(w))
			if err != nil {
				t.Fatal(err)
			}
			if p.Status != StatusCandidate || p.Evidence != "" || p.Workflow == nil || len(p.Workflow.Reports) != 1 ||
				p.Workflow.Candidate != w.Candidate || p.Workflow.Applicability != w.Applicability || p.Workflow.Reports[0] != w.Reports[0] ||
				!strings.Contains(p.Body, "not verified authority") || !strings.Contains(p.Body, "todo:cdae01f261b1") {
				t.Fatalf("lost provenance or crossed validation boundary: %+v", p)
			}
			pages, _ = Open(dir).List()
			if len(pages) != 1 {
				t.Fatal("duplicate report created pages")
			}
			// Untrusted import cannot append to an explicitly validated page.
			mustRun(t, dir, "validate", p.Slug, "--evidence", "review:synthetic-fixture")
			w.Reports[0].Summary = "new claim"
			b, _ = json.Marshal(w)
			if _, err := run(t, dir, string(b), "workflow", "--apply"); err == nil {
				t.Fatal("import changed validated knowledge")
			}
		})
	}
}
