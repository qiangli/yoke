package coord

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEvidenceChild(t *testing.T) {
	if os.Getenv("COORD_EVIDENCE_CHILD") != "1" {
		return
	}
	dir := os.Getenv("BASHY_CLAIM_EVIDENCE_DIR")
	if dir == "" || os.Getenv("BASHY_CLAIM_EPOCH") == "" {
		os.Exit(11)
	}
	if err := os.WriteFile(filepath.Join(dir, "result"), []byte(os.Getenv("BASHY_CLAIM_CANDIDATE")), 0600); err != nil {
		os.Exit(12)
	}
	os.Stdout.WriteString("candidate=" + os.Getenv("BASHY_CLAIM_CANDIDATE"))
	os.Exit(0)
}

func TestCompetingCandidateEvidence(t *testing.T) {
	dir := ledger(t)
	base := t.TempDir()
	t.Setenv("COORD_EVIDENCE_CHILD", "1")
	// Inherited evidence variables must never redirect a later run's artifacts.
	t.Setenv("BASHY_CLAIM_EVIDENCE_DIR", base)
	type result struct{ err error }
	done := make(chan result, 2)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"candidate-A", "candidate-B"} {
		go func(candidate string) {
			g, l, err := AcquireAttachedRef(context.Background(), Request{Ref: ParseRef("test-host"), Holder: queueOwner(candidate), Wait: 3 * time.Second})
			if err != nil {
				done <- result{err}
				return
			}
			child := exec.Command(exe, "-test.run=^TestEvidenceChild$")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			err = runEvidence(child, g.Claim, base, candidate)
			releaseErr := ReleaseAttached(dir, g.Claim, l)
			if err == nil {
				err = releaseErr
			}
			done <- result{err}
		}(candidate)
	}
	for range 2 {
		if r := <-done; r.err != nil {
			t.Fatal(r.err)
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 2 {
		t.Fatalf("attempt dirs: %v %v", entries, err)
	}
	seen := map[string]bool{}
	epochs := map[uint64]bool{}
	for _, e := range entries {
		runDir := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(runDir, "run.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Candidate string
			Claim     *Claim
			Status    string
		}
		if err := json.Unmarshal(b, &record); err != nil {
			t.Fatal(err)
		}
		result, err := os.ReadFile(filepath.Join(runDir, "result"))
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := os.ReadFile(filepath.Join(runDir, "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		if string(result) != record.Candidate || string(stdout) != "candidate="+record.Candidate || record.Status != "completed" {
			t.Fatalf("contaminated evidence: %s %s %s", b, result, stdout)
		}
		seen[record.Candidate] = true
		epochs[record.Claim.Epoch] = true
	}
	if len(seen) != 2 || len(epochs) != 2 {
		t.Fatalf("reused identity: %v %v", seen, epochs)
	}
}
