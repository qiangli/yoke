package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Observed live (run #139): the reaper wrote "workspace preserved, `weave start
// --resume --issue 139` to retry" and doctor's NEXT STEP echoed it — but there
// was NO workspace, so the one advertised recovery path refused
// ("--resume: run #139 has no workspace to reattach"). The run was
// unrecoverable-by-instruction: the operator had to guess `weave start --run
// 139 -- <agent>`, which does work.
//
// Both the comment and the NEXT STEP must be derived from the workspace that is
// ACTUALLY on disk, never asserted.

func TestReaperNamesFreshStartWhenWorkspaceGone(t *testing.T) {
	// A workspace path that was recorded and then deleted — the wrapper died
	// mid-teardown, or a prune removed it.
	gone := filepath.Join(t.TempDir(), "workspaces", "issue-1")

	q := &weaveQueue{Items: []*weaveItem{{
		ID: 1, Title: "wrapper died, workspace gone", State: "working",
		WrapperPid: deadPID(t), Workspace: gone,
		StartedAt: time.Now().Add(-2 * time.Hour),
	}}}

	weaveReapPass(q, "", "", time.Now().UTC())

	it := q.Items[0]
	if it.State != "failed" {
		t.Fatalf("state = %q, want failed", it.State)
	}
	body := lastReaperComment(t, it)
	if strings.Contains(body, "workspace preserved") {
		t.Errorf("reaper claimed a preserved workspace that does not exist: %q", body)
	}
	if strings.Contains(body, "--resume") {
		t.Errorf("reaper advertised --resume, which refuses without a workspace: %q", body)
	}
	if !strings.Contains(body, "weave start --run 1 -- <agent>") {
		t.Errorf("reaper comment must name the fresh-start command, got %q", body)
	}
	if !strings.Contains(body, "workspace lost") {
		t.Errorf("reaper comment must say the workspace is lost, got %q", body)
	}

	step := weaveNextSteps(it)
	if strings.Contains(step, "--resume") {
		t.Errorf("doctor NEXT STEP advertised --resume with no workspace: %q", step)
	}
	if !strings.HasPrefix(step, "failed -> allocated") || !strings.Contains(step, "weave start --run 1 -- <agent>") {
		t.Errorf("doctor NEXT STEP = %q, want failed -> allocated (weave start --run 1 -- <agent>)", step)
	}
}

// The converse: a workspace that IS on disk keeps the resume advice, because
// --resume genuinely reattaches there.
func TestReaperKeepsResumeWhenWorkspaceOnDisk(t *testing.T) {
	live := filepath.Join(t.TempDir(), "workspaces", "issue-2")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}

	q := &weaveQueue{Items: []*weaveItem{{
		ID: 2, Title: "wrapper died, workspace intact", State: "working",
		WrapperPid: deadPID(t), Workspace: live,
		StartedAt: time.Now().Add(-2 * time.Hour),
	}}}

	weaveReapPass(q, "", "", time.Now().UTC())

	it := q.Items[0]
	body := lastReaperComment(t, it)
	if !strings.Contains(body, "workspace preserved") || !strings.Contains(body, "--resume --issue 2") {
		t.Errorf("comment = %q, want the resume path for a workspace that exists", body)
	}
	if step := weaveNextSteps(it); !strings.Contains(step, "--resume") {
		t.Errorf("NEXT STEP = %q, want the resume transition for a live workspace", step)
	}
}

// A file where the workspace should be is not a workspace either.
func TestWorkspaceLiveRejectsNonDirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "issue-3")
	if err := os.WriteFile(f, []byte("not a workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		it   *weaveItem
		want bool
	}{
		{"unrecorded", &weaveItem{ID: 3}, false},
		{"file", &weaveItem{ID: 3, Workspace: f}, false},
		{"dir", &weaveItem{ID: 3, Workspace: filepath.Dir(f)}, true},
	} {
		if got := weaveWorkspaceLive(tc.it); got != tc.want {
			t.Errorf("%s: weaveWorkspaceLive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func lastReaperComment(t *testing.T, it *weaveItem) string {
	t.Helper()
	for i := len(it.Comments) - 1; i >= 0; i-- {
		if it.Comments[i].Author == "reaper" {
			return it.Comments[i].Body
		}
	}
	t.Fatalf("no reaper comment recorded on #%d", it.ID)
	return ""
}
