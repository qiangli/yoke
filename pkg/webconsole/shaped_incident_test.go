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

// S549.R1e rework: one root cause is one incident whatever the script
// order. A page whose inline script fetches api/list BEFORE genie.js wraps
// fetch throws on the 500 without the client ever seeing an incident id, so
// the follow-on script report must fold server-side into the open api
// incident: no new incident, the api incident's id returned.
func TestIncidentScriptFoldsIntoRecentAPIIncident(t *testing.T) {
	s := testIncidents(t)
	now := time.Unix(1_800_000_000, 0)
	api, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed (exit 1)", BaseRev: "r0"}, now)
	if err != nil || !start {
		t.Fatalf("api observe: %+v start=%v err=%v", api, start, err)
	}
	// A script error a second later is the fetch's fallout: folded.
	folded, start, err := s.Observe(Incident{App: "notes", Kind: IncidentScript, Path: "/", Message: "TypeError: list failed"}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if start {
		t.Fatal("folded script report started a repair")
	}
	if folded.ID != api.ID {
		t.Fatalf("folded into %q, want the api incident %q", folded.ID, api.ID)
	}
	if folded.Count != 2 {
		t.Fatalf("folded count = %d, want 2", folded.Count)
	}
	if !folded.LastSeen.Equal(now.Add(time.Second)) {
		t.Fatalf("folded last seen = %v", folded.LastSeen)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("incidents = %d, want 1: %+v", len(list), list)
	}
	// Outside the window it is a new incident.
	later, _, err := s.Observe(Incident{App: "notes", Kind: IncidentScript, Path: "/", Message: "TypeError: list failed"}, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if later.ID == api.ID {
		t.Fatalf("script report 30s later folded into %q", api.ID)
	}
	list, _ = s.List()
	if len(list) != 2 {
		t.Fatalf("incidents = %d, want 2: %+v", len(list), list)
	}
	// Asset reports never fold, even inside the window.
	asset, _, err := s.Observe(Incident{App: "notes", Kind: IncidentAsset, Path: "/", Message: "asset failed"}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if asset.ID == api.ID {
		t.Fatalf("asset report folded into %q", api.ID)
	}
	// An offline (external) script report never folds either.
	ext, _, err := s.Observe(Incident{App: "notes", Kind: IncidentScript, Path: "/", Message: "offline", State: IncidentExternal}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ext.ID == api.ID {
		t.Fatalf("offline report folded into %q", api.ID)
	}
}

func TestIncidentScriptFoldIsPerPrincipal(t *testing.T) {
	st, err := shape.OpenAt(t.TempDir(), "notes", "cloud:bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	other, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	s := testIncidents(t)
	api, _, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", BaseRev: "r0"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Another principal's store knows nothing of the api incident: new one.
	got, _, err := other.Observe(Incident{App: "notes", Kind: IncidentScript, Path: "/", Message: "TypeError: list failed"}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == api.ID {
		t.Fatalf("cross-principal fold into %q", api.ID)
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
