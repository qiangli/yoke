package weave

import (
	"os"
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
	msg := weaveAgentBusyErr("elif", item(3, "working", "elif", 1), item(9, "todo", "", 0)).Error()
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
