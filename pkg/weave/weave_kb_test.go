package weave

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/weave/memory"
)

func isolateWeaveKBStores(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(root, "host-kb"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "bashy-home"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(root, "ycode"))
	return root
}

func TestWeaveInjectKBFile(t *testing.T) {
	isolateWeaveKBStores(t)
	kbDir := os.Getenv("BASHY_KB_DIR")
	store := kb.Open(kbDir)
	if err := store.Write(&kb.Page{
		Slug: "codesign-dance", Type: kb.TypeGotcha,
		Title:       "codesign dance on macOS",
		Description: "WHEN swapping a running signed binary",
		Body:        "rm, cp, codesign --force.",
	}, "add"); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	it := &weaveItem{ID: 1, Title: "fix the codesign failure on upgrade"}
	if err := weaveInjectKBFile("/home/x/.bashy/weave/myrepo-0a1b2c3d", workspace, it); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(workspace, "KB.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "codesign-dance") {
		t.Fatalf("KB.md missing the matching page:\n%s", got)
	}
	if !strings.Contains(got, "bashy kb retro") {
		t.Fatalf("KB.md missing the write-back instruction:\n%s", got)
	}
	// The drop must be git-excluded so it can never merge.
	excl, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(excl), "KB.md") {
		t.Fatalf("KB.md not git-excluded:\n%s", excl)
	}
}

func TestWeaveInjectKBFileEmptyStore(t *testing.T) {
	isolateWeaveKBStores(t)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	it := &weaveItem{ID: 2, Title: "anything"}
	if err := weaveInjectKBFile("/home/x/.bashy/weave/myrepo-0a1b2c3d", workspace, it); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(workspace, "KB.md"))
	if err != nil {
		t.Fatal(err)
	}
	// No matches: still carries the contribute + retro instructions.
	if !strings.Contains(string(b), "contribute") || !strings.Contains(string(b), "bashy kb retro") {
		t.Fatalf("empty-store KB.md missing loop instructions:\n%s", b)
	}
}

func TestWeaveRepoNameFromQueueDir(t *testing.T) {
	isolateWeaveKBStores(t)
	if got := weaveRepoNameFromQueueDir("/h/.bashy/weave/myrepo-0a1b2c3d"); got != "myrepo" {
		t.Fatalf("got %q", got)
	}
	if got := weaveRepoNameFromQueueDir("/h/.bashy/weave/odd-name"); got != "odd-name" {
		t.Fatalf("got %q", got)
	}
}

func TestWeaveInjectKBFileReadsRepoRingWithinBudget(t *testing.T) {
	root := isolateWeaveKBStores(t)
	repo := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := kb.Open(filepath.Join(repo, kb.RepoSub)).Write(&kb.Page{
		Slug: "repo-parser", Form: kb.FormPage, Type: kb.TypeLesson,
		Title: "parser recovery", Description: "recover parser state after invalid input",
		Body: strings.Repeat("parser recovery details ", 100),
	}, "add"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := weaveInjectKBFile("/state/myrepo-0a1b2c3d", workspace,
		&weaveItem{ID: 3, Title: "fix parser recovery"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(workspace, weaveKBFileName))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "kb:repo-parser") || !strings.Contains(got, "[repo/page]") {
		t.Fatalf("repo-ring page did not reach KB.md:\n%s", got)
	}
	start := strings.Index(got, "- [repo/page]")
	if start < 0 {
		t.Fatalf("cannot locate assembled context in KB.md:\n%s", got)
	}
	end := strings.Index(got[start:], "\n\nMore:")
	if end < 0 {
		t.Fatalf("cannot locate assembled context in KB.md:\n%s", got)
	}
	if n := len(got[start : start+end]); n > weaveKBBudget {
		t.Fatalf("assembled KB context is %d bytes, budget %d", n, weaveKBBudget)
	}
}

func TestRunDrainGateWritesValidatableEventAndObservation(t *testing.T) {
	root := isolateWeaveKBStores(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	out := runDrainGate(context.Background(), repo, "go version")
	if !out.Ran || !out.Passed || out.GateEventID == "" {
		t.Fatalf("gate outcome missing event: %+v", out)
	}
	lines, err := kb.Open(os.Getenv("BASHY_KB_DIR")).JournalTail(0)
	if err != nil {
		t.Fatal(err)
	}
	gateEvents := 0
	for _, line := range lines {
		var event struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Kind == "gate" {
			gateEvents++
			if event.ID != out.GateEventID {
				t.Fatalf("journal id %q != outcome id %q", event.ID, out.GateEventID)
			}
		}
	}
	if gateEvents != 1 {
		t.Fatalf("kind=gate events = %d, want 1", gateEvents)
	}

	store := kb.Open(os.Getenv("BASHY_KB_DIR"))
	if err := store.Write(&kb.Page{Slug: "candidate", Type: kb.TypeLesson, Status: kb.StatusCandidate, Title: "candidate"}, "add"); err != nil {
		t.Fatal(err)
	}
	cmd := kb.NewKBCmd()
	cmd.SetArgs([]string{"--dir", os.Getenv("BASHY_KB_DIR"), "validate", "candidate", "--from-gate", out.GateEventID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate --from-gate rejected drain event: %v", err)
	}

	queueDir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	mem, _, err := memory.Open(queueDir, memory.Prefs{})
	if err != nil {
		t.Fatal(err)
	}
	observations, err := mem.Recall(context.Background(), memory.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].GateEventID != out.GateEventID {
		t.Fatalf("gate event id not discoverable in run observation: %+v", observations)
	}
}
