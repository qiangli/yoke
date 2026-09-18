package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommitTrace(t *testing.T) {
	message := `feat(board): make progress explicit

The subject and body stay useful to humans.

Sprint: #87
Story: #110
Story-ID: d1e86f29d7a7
Story: #111
Story-ID: a01706e3260f
Signed-off-by: Example <example@example.test>
# Please enter the commit message for your changes. Lines starting with '#'
# are removed by Git after this hook runs.
`
	trace, err := parseCommitTrace(message)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Sprint != 87 || len(trace.Stories) != 2 {
		t.Fatalf("trace = %+v", trace)
	}
	if trace.Stories[0].Number != 110 || trace.Stories[0].ID != "d1e86f29d7a7" {
		t.Fatalf("first story = %+v", trace.Stories[0])
	}
}

func TestParseCommitTraceRefusals(t *testing.T) {
	tests := []struct {
		name, message, want string
	}{
		{"no trailers", "fix: untracked", "subject, a blank line"},
		{"duplicate sprint", "fix: x\n\nSprint: #87\nSprint: #88\nStory: #110\nStory-ID: d1e86f29d7a7", "exactly one"},
		{"missing stable id", "fix: x\n\nSprint: #87\nStory: #110", "matching Story-ID"},
		{"short stable id", "fix: x\n\nSprint: #87\nStory: #110\nStory-ID: d1e86f29", "full 12-character"},
		{"uppercase stable id", "fix: x\n\nSprint: #87\nStory: #110\nStory-ID: D1E86F29D7A7", "lowercase hex"},
		{"bad story marker", "fix: x\n\nSprint: #87\nStory: 110\nStory-ID: d1e86f29d7a7", "Story: #110"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCommitTrace(tt.message)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateCommitTraceStories(t *testing.T) {
	stories := []sprintStoryState{
		{Seq: 110, Ref: sprintStoryRef{ID: "d1e86f29d7a7"}},
		{Seq: 111, Ref: sprintStoryRef{ID: "a01706e3260f"}},
	}
	good := commitTrace{Sprint: 87, Stories: []commitStoryRef{{Number: 110, ID: "d1e86f29d7a7"}}}
	if err := validateCommitTraceStories(good, stories); err != nil {
		t.Fatal(err)
	}
	bad := commitTrace{Sprint: 87, Stories: []commitStoryRef{{Number: 110, ID: "a01706e3260f"}}}
	if err := validateCommitTraceStories(bad, stories); err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("mismatched pair err = %v", err)
	}
	missing := commitTrace{Sprint: 97, Stories: []commitStoryRef{{Number: 16, ID: "0123456789ab"}}}
	if err := validateCommitTraceStories(missing, nil); err == nil ||
		!strings.Contains(err.Error(), "todo add --sprint 97") ||
		!strings.Contains(err.Error(), "weave run is execution") {
		t.Fatalf("missing-story remedy err = %v", err)
	}
}

func TestManagedCommitHookChainsAndValidates(t *testing.T) {
	for _, want := range []string{"commit-msg.before-bashy", `bashy sprint commit-msg "$1"`} {
		if !strings.Contains(managedCommitHook, want) {
			t.Errorf("managed hook missing %q", want)
		}
	}
}

func TestSprintHelpStatesTrackingAndOwnershipContract(t *testing.T) {
	help := NewSprintCmd().Long
	for _, want := range []string{
		"create and link a sprint story before implementation begins",
		"Sprint: #87",
		"bashy sprint hooks install",
		"receives the inbox automatically under the",
		"recorded owner name",
		"Never delete or destroy work owned by another agent",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("sprint help missing %q", want)
		}
	}
}

func TestSprintCommitMsgPrintsActionableRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte("fix: untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"commit-msg", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("invalid commit message passed")
	}
	for _, want := range []string{"commit provenance", "Sprint: #87", "Story-ID: d1e86f29d7a7"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal missing %q:\n%s", want, out.String())
		}
	}
}

func TestInstallSprintCommitHookPreservesExistingHooks(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")
	hooks := filepath.Join(repo, "project-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	prePush := filepath.Join(hooks, "pre-push")
	if err := os.WriteFile(prePush, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "--local", "core.hooksPath", hooks)

	root, err := installSprintCommitHook(repo)
	if err != nil {
		t.Fatal(err)
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	if root != realRepo {
		t.Fatalf("root = %q, want %q", root, realRepo)
	}
	managed := git("config", "--local", "--get", "core.hooksPath")
	if managed == hooks || !strings.HasSuffix(managed, "bashy-hooks") {
		t.Fatalf("managed hooks path = %q", managed)
	}
	for _, name := range []string{"pre-push", "commit-msg"} {
		info, err := os.Stat(filepath.Join(managed, name))
		if err != nil || info.Mode()&0o111 == 0 {
			t.Fatalf("preserved %s hook = info:%v err:%v", name, info, err)
		}
	}
}
