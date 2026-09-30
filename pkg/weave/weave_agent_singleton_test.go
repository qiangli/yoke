package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func item(id int64, state, agent string, wrapperPid int) *weaveItem {
	it := &weaveItem{ID: id, State: state, Title: "run " + state, WrapperPid: wrapperPid}
	if agent != "" {
		it.LaunchSpec = &weaveLaunchSpec{Tool: agent, Agent: agent}
	}
	return it
}

// TestAgentWorkingOnFindsTheLiveRun — the check that makes one agent take one
// issue at a time. os.Getpid() stands in for a live wrapper.
func TestAgentWorkingOnFindsTheLiveRun(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{
		item(1, "working", "elif", os.Getpid()),
		item(2, "todo", "", 0),
	}}
	busy := weaveAgentWorkingOn(q, "elif", 2)
	if busy == nil || busy.ID != 1 {
		t.Fatalf("weaveAgentWorkingOn = %v, want run #1", busy)
	}
	// Case-insensitively, since an agent may be named either way on the CLI.
	if got := weaveAgentWorkingOn(q, "ELIF", 2); got == nil {
		t.Error("agent lookup must be case-insensitive")
	}
	// A different agent is not busy.
	if got := weaveAgentWorkingOn(q, "bruno", 2); got != nil {
		t.Errorf("weaveAgentWorkingOn(bruno) = %v, want nil", got)
	}
}

// TestAgentWorkingOnIgnoresDeadAndFinished is what keeps a crashed run from
// blocking its agent forever. A dead wrapper's issue is stale state, not a busy
// agent — the recovery paths already reclaim it, and this must not pre-empt them
// by refusing every future start.
func TestAgentWorkingOnIgnoresDeadAndFinished(t *testing.T) {
	const deadPid = 2147483000
	for _, tc := range []struct {
		name string
		it   *weaveItem
	}{
		{"dead wrapper", item(1, "working", "elif", deadPid)},
		{"no wrapper yet", item(1, "working", "elif", 0)},
		{"done", item(1, "done", "elif", os.Getpid())},
		{"abandoned", item(1, "abandoned", "elif", os.Getpid())},
		{"still queued", item(1, "todo", "elif", os.Getpid())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &weaveQueue{Items: []*weaveItem{tc.it}}
			if got := weaveAgentWorkingOn(q, "elif", 99); got != nil {
				t.Errorf("run in state %q (pid %d) counted as busy", tc.it.State, tc.it.WrapperPid)
			}
		})
	}
}

// TestAgentWorkingOnSkipsTheRunBeingStarted — a restart of the SAME issue is not
// the agent competing with itself.
func TestAgentWorkingOnSkipsTheRunBeingStarted(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{item(7, "working", "elif", os.Getpid())}}
	if got := weaveAgentWorkingOn(q, "elif", 7); got != nil {
		t.Errorf("run #7 must not block starting run #7, got %v", got)
	}
}

// TestAgentBusyErrNamesBothWaysForward — a refusal that only says "no" is how an
// operator learns to reach for --force.
func TestAgentBusyErrNamesBothWaysForward(t *testing.T) {
	msg := weaveAgentBusyErr("elif", "", item(3, "working", "elif", 1), item(9, "todo", "", 0)).Error()
	for _, want := range []string{"elif", "#3", "#9", "--clone", "queued"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q:\n%s", want, msg)
		}
	}
}

// TestIssueCloneNameIsUsableAsAnAgentName — the minted worker's name becomes a
// catalog filename, so it must be one the registry will accept.
func TestIssueCloneNameIsUsableAsAnAgentName(t *testing.T) {
	got := weaveIssueCloneName("elif", 412)
	if got != "elif-w412" {
		t.Errorf("weaveIssueCloneName = %q, want elif-w412", got)
	}
	if strings.ContainsAny(got, `/\ `) {
		t.Errorf("clone name %q is not filename-safe", got)
	}
}

func setupNamedRepo(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "seed.txt")
	gitT(t, dir, "commit", "-qm", "seed")
	root, err := weaveRepoRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestStartRefusesAgentLiveInAnotherQueue — the identity is fleet-wide, so the
// busy check reads every queue on the host, not just the one being started in.
// The refusal names the run as repo#id (a bare #1 would read as this queue's),
// and --clone stays the way to run in parallel.
func TestStartRefusesAgentLiveInAnotherQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Weave Test\n\temail = weave-test@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pinAgentFleet(t)

	repoA := setupNamedRepo(t, home, "repo-alpha")
	repoB := setupNamedRepo(t, home, "repo-beta")

	dirA, err := weaveQueueDir(repoA)
	if err != nil {
		t.Fatal(err)
	}
	qA := &weaveQueue{
		Root: repoA,
		Items: []*weaveItem{
			{
				ID:         1,
				Title:      "alpha task",
				State:      "working",
				WrapperPid: os.Getpid(),
				LaunchSpec: &weaveLaunchSpec{Tool: "claude", Agent: "007"},
			},
		},
	}
	if err := saveWeaveQueue(dirA, qA); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repoB)
	if out, code := runWeave(t, "add", "beta task", "--body", "body", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}

	out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "007")
	if code == 0 {
		t.Fatalf("weave start must refuse agent live in another queue, got exit 0, output: %s", out)
	}
	if !strings.Contains(out, "repo-alpha#1") {
		t.Errorf("refusal output must name repo#id (repo-alpha#1), got:\n%s", out)
	}
	if !strings.Contains(out, "agent 007 is already working run repo-alpha#1") {
		t.Errorf("refusal output must explain agent is working run repo-alpha#1, got:\n%s", out)
	}

	dirB, err := weaveQueueDir(repoB)
	if err != nil {
		t.Fatal(err)
	}
	qB, err := loadWeaveQueue(dirB)
	if err != nil {
		t.Fatal(err)
	}
	if it := findWeaveItem(qB, 1); it == nil || it.State != "todo" {
		t.Fatalf("a refused run must stay queued: %+v", it)
	}

	// --clone still allowed
	out, code = runWeave(t, "start", "--run", "1", "--clone", "--no-spawn", "--tool", "007")
	if code != 0 {
		t.Fatalf("weave start --clone must succeed, got exit %d, output: %s", code, out)
	}
	if !strings.Contains(out, "007 is on repo-alpha#1") {
		t.Errorf("clone notice must name the other queue's run (repo-alpha#1), got:\n%s", out)
	}
	if qB, err = loadWeaveQueue(dirB); err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(qB, 1)
	if it == nil || it.LaunchSpec == nil || it.LaunchSpec.Agent != "007-w1" {
		t.Fatalf("clone worker not allocated as 007-w1: %+v", it)
	}
}
