package weave

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWeaveReportTerminalCreatesAndCachesSprint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDBOX_TOKEN", "test-token")
	repoRoot := t.TempDir()
	fake := &fakeSessionClient{}
	old := newSessionClient
	newSessionClient = func(base, token string) SessionClient {
		if base != "https://cloudbox.test" || token != "test-token" {
			t.Fatalf("client base/token = %q/%q", base, token)
		}
		return fake
	}
	t.Cleanup(func() { newSessionClient = old })

	if err := WriteSessionPointer(repoRoot, &SessionPointer{TaskID: "task-1", CloudboxBase: "https://cloudbox.test"}); err != nil {
		t.Fatal(err)
	}
	exit := 0
	verifyExit := 0
	it := &weaveItem{
		ID:           13,
		Title:        "ship reporter",
		State:        "submitted",
		Tool:         "codex",
		Workspace:    "/tmp/workspace",
		Branch:       "agent/weave-issue-13",
		CommitsAhead: 2,
		ExitCode:     &exit,
		VerifyExit:   &verifyExit,
		VerifyOutput: "ok\nall green\n",
	}
	ev := weaveTerminalEvidence{CommitsAhead: 2, VerifyExit: &verifyExit, VerifyOutput: it.VerifyOutput}

	if err := weaveReportTerminal(context.Background(), repoRoot, it, ev); err != nil {
		t.Fatal(err)
	}
	if err := weaveReportTerminal(context.Background(), repoRoot, it, ev); err != nil {
		t.Fatal(err)
	}
	if len(fake.sprints) != 1 {
		t.Fatalf("CreateSprint calls = %d, want 1", len(fake.sprints))
	}
	if fake.sprints[0].TaskID != "task-1" || fake.sprints[0].TargetRepo != repoRoot || fake.sprints[0].Gate != "weave verify" {
		t.Fatalf("CreateSprint req = %+v", fake.sprints[0])
	}
	ptr, err := ReadSessionPointer(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if ptr == nil || ptr.SprintID != "sprint-1" {
		t.Fatalf("pointer SprintID = %+v", ptr)
	}
	if len(fake.runs) != 2 || len(fake.runSprints) != 2 || fake.runSprints[0] != "sprint-1" || fake.runSprints[1] != "sprint-1" {
		t.Fatalf("UpsertRun calls = runs:%+v sprintIDs:%+v", fake.runs, fake.runSprints)
	}
	run := fake.runs[0]
	if run.Issue != "#13 ship reporter" || run.Agent != "codex" || run.Status != "verified" || run.CommitsAhead != 2 || run.Sandbox != "/tmp/workspace" {
		t.Fatalf("UpsertRun req = %+v", run)
	}
	if run.Tool != "codex" || !strings.Contains(run.Verdict, "all green") {
		t.Fatalf("UpsertRun verdict/tool = %+v", run)
	}
	if len(fake.appends) != 2 || fake.appendTasks[0] != "task-1" || fake.appends[0].Kind != "verdict" {
		t.Fatalf("AppendEvent calls = tasks:%+v appends:%+v", fake.appendTasks, fake.appends)
	}
	var detail map[string]any
	if err := json.Unmarshal(fake.appends[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["sprint_id"] != "sprint-1" || detail["sandbox"] != "/tmp/workspace" || detail["status"] != "verified" {
		t.Fatalf("AppendEvent detail = %+v", detail)
	}
}

func TestWeaveReportTerminalNoSessionPointerNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()
	called := false
	old := newSessionClient
	newSessionClient = func(base, token string) SessionClient {
		called = true
		return &fakeSessionClient{}
	}
	t.Cleanup(func() { newSessionClient = old })

	if err := weaveReportTerminal(context.Background(), repoRoot, &weaveItem{ID: 1}, weaveTerminalEvidence{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatalf("newSessionClient was called without a session pointer")
	}
}
