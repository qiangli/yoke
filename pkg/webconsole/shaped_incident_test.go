package webconsole

import (
	"errors"
	"testing"
	"time"

	shape "github.com/qiangli/yoke/pkg/shape/state"
)

func testIncidents(t *testing.T) *incidentStore {
	t.Helper()
	st, err := shape.OpenAt(t.TempDir(), "notes", "cloud:alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIncidentSignatureFoldsDigits(t *testing.T) {
	a := IncidentSignature(IncidentAPI, "api/list", "unit list failed (exit 1)\nstack")
	b := IncidentSignature(IncidentAPI, "api/list", "unit list failed (exit 2)")
	if a != b {
		t.Fatalf("same error split by digits: %s vs %s", a, b)
	}
	if a == IncidentSignature(IncidentAPI, "api/save", "unit list failed (exit 1)") {
		t.Fatal("different paths share a signature")
	}
}

// Two attempts at most per incident; the same error after an activated
// repair re-opens it, and after the second attempt it is failed — no loop.
func TestIncidentBoundedAttempts(t *testing.T) {
	s := testIncidents(t)
	now := time.Unix(1_800_000_000, 0)
	in, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed (exit 1)", BaseRev: "r0"}, now)
	if err != nil || !start || in.State != IncidentRepairing {
		t.Fatalf("first observe: %+v start=%v err=%v", in, start, err)
	}
	// A repeat while repairing does not start a second repair.
	if _, start, _ := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Message: "unit list failed (exit 1)"}, now); start {
		t.Fatal("repeat while repairing started another repair")
	}
	if in, err = s.Attempt(in.ID, RepairAttempt{Candidate: "c1", Result: "activated", Revision: "r1", At: now}); err != nil || in.State != IncidentActivated {
		t.Fatalf("attempt 1: %+v %v", in, err)
	}
	// The repair did not hold.
	in, start, _ = s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Message: "unit list failed (exit 1)"}, now)
	if !start || in.State != IncidentRepairing {
		t.Fatalf("recurrence after repair: %+v start=%v", in, start)
	}
	if in, _ = s.Attempt(in.ID, RepairAttempt{Candidate: "c2", Result: "failed", Reason: "verifier", At: now}); in.State != IncidentFailed {
		t.Fatalf("attempt 2 failed should fail the incident: %+v", in)
	}
	if _, err := s.Attempt(in.ID, RepairAttempt{Candidate: "c3", Result: "activated"}); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("third attempt: %v", err)
	}
	if _, start, _ := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Message: "unit list failed (exit 1)"}, now); start {
		t.Fatal("failed incident started a repair")
	}
}

func TestIncidentRefusedIsLeftForReview(t *testing.T) {
	s := testIncidents(t)
	now := time.Unix(1_800_000_000, 0)
	in, _, _ := s.Observe(Incident{App: "notes", Kind: IncidentScript, Path: "index.html", Message: "TypeError: x is undefined"}, now)
	in, err := s.Attempt(in.ID, RepairAttempt{Candidate: "c1", Result: "refused", Reason: "adds api/export"})
	if err != nil || in.State != IncidentRefused {
		t.Fatalf("refused: %+v %v", in, err)
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Attempts[0].Reason != "adds api/export" {
		t.Fatalf("list: %+v", list)
	}
}

func TestIncidentEvidenceBoundedAndIDSafe(t *testing.T) {
	s := testIncidents(t)
	big := make([]byte, incidentEvidenceLimit*3)
	for i := range big {
		big[i] = 'x'
	}
	in, _, _ := s.Observe(Incident{App: "notes", Kind: IncidentServer, Path: "", Message: "502", Evidence: string(big)}, time.Now())
	if len(in.Evidence) != incidentEvidenceLimit {
		t.Fatalf("evidence %d bytes", len(in.Evidence))
	}
	if _, err := s.Get("../state"); err == nil {
		t.Fatal("path traversal id accepted")
	}
}
