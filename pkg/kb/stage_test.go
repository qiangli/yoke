package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scratchStageEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_KB_DIR", filepath.Join(t.TempDir(), "kb"))
	t.Setenv("BASHY_HOME", filepath.Join(t.TempDir(), "bashy-home"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(t.TempDir(), "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(t.TempDir(), "ycode"))
}

func TestObserveWritesJournalEventOnlyAndStableID(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	args := []string{
		"observe",
		"--episode", "ep-1",
		"--kind", "note",
		"--ref", "payload-1",
		"--summary", "saw a thing",
		"--at", "2026-09-12T00:00:00Z",
	}
	out1 := mustRun(t, dir, args...)
	id1 := strings.TrimSpace(out1)
	if id1 == "" {
		t.Fatal("observe did not print an event id")
	}
	lines, err := Open(dir).JournalTail(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("observe should append one journal line, got %d: %v", len(lines), lines)
	}
	if _, err := os.Stat(filepath.Join(dir, "pages")); !os.IsNotExist(err) {
		t.Fatalf("observe should not create pages, stat err=%v", err)
	}

	dir2 := t.TempDir()
	out2 := mustRun(t, dir2, args...)
	if id2 := strings.TrimSpace(out2); id2 != id1 {
		t.Fatalf("event id not stable: %q != %q", id2, id1)
	}
}

func TestValidateFromGateMissingWritesNothing(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	mustRun(t, dir, "note", "add", "--candidate", "--title", "candidate note", "--body", "body")
	path := filepath.Join(dir, "pages", "candidate-note.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, dir, "", "validate", "candidate-note", "--from-gate", "missing-event"); err == nil || !strings.Contains(err.Error(), "no such observe event") {
		t.Fatalf("expected no-such-event validation failure, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed validate mutated the page\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestValidateFromGateRejectedWhenGateDidNotPass(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	mustRun(t, dir, "note", "add", "--candidate", "--title", "candidate note", "--body", "body")
	id := strings.TrimSpace(mustRun(t, dir, "observe",
		"--episode", "ep-1",
		"--kind", "gate",
		"--ref", "gate-payload",
		"--ran",
		"--command", "go test ./pkg/kb/...",
		"--exit-code", "1",
		"--where", "workspace",
		"--at", "2026-09-12T00:00:00Z"))
	if _, err := run(t, dir, "", "validate", "candidate-note", "--from-gate", id); err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("expected did-not-pass validation failure, got %v", err)
	}
	p, err := Open(dir).Load("candidate-note")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusCandidate {
		t.Fatalf("failed gate should leave status candidate, got %s", p.Status)
	}
}

func TestValidateFromGatePromotesPassedGate(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	mustRun(t, dir, "note", "add", "--candidate", "--title", "candidate note", "--body", "body")
	id := strings.TrimSpace(mustRun(t, dir, "observe",
		"--episode", "ep-1",
		"--kind", "gate",
		"--ref", "gate-payload",
		"--ran",
		"--passed",
		"--command", "go test ./pkg/kb/...",
		"--where", "workspace",
		"--at", "2026-09-12T00:00:00Z"))
	mustRun(t, dir, "validate", "candidate-note", "--from-gate", id)
	p, err := Open(dir).Load("candidate-note")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusValidated || p.Evidence != "gate:"+id {
		t.Fatalf("passed gate did not validate with gate evidence: %+v", p)
	}
}

func TestNoteAddNearDuplicateNamesExistingSlug(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	mustRun(t, dir, "note", "add", "--candidate", "--title", "gate event validates candidates", "--body", "first")
	_, err := run(t, dir, "", "note", "add", "--candidate", "--title", "gate event validates candidate", "--body", "second")
	if err == nil {
		t.Fatal("near duplicate note add should fail")
	}
	if !strings.Contains(err.Error(), "gate-event-validates-candidates") {
		t.Fatalf("duplicate error should name existing slug, got %v", err)
	}
}

func TestNoteAddRefusesHostnameInShareableRingOnly(t *testing.T) {
	scratchStageEnv(t)
	dir := t.TempDir()
	host := HostID()
	if host == "" {
		t.Fatal("HostID returned empty")
	}
	_, err := run(t, dir, "", "note", "add", "--candidate", "--title", "host fact", "--body", "the host is "+host)
	if err == nil || !strings.Contains(err.Error(), "refusing shareable-ring write") {
		t.Fatalf("repo/host note should refuse hostname body, got %v", err)
	}
	t.Chdir(t.TempDir())
	if _, _, err := runRing(t, "", "--ring", "agent", "note", "add", "--candidate", "--title", "host fact", "--body", "the host is "+host); err != nil {
		t.Fatalf("agent note should allow hostname body: %v", err)
	}
}
