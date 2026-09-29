package weave

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestCommitAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, agent, env, assignee, mode string
		grandfather, bad                 bool
	}{
		{name: "matching", agent: "agent-a", assignee: "agent-a"},
		{name: "binding", agent: "tool-a:model-a", assignee: "agent-a"},
		{name: "fallback", env: "agent-a", assignee: "agent-a"},
		{name: "clone", agent: "agent-a-w25", assignee: "agent-a"},
		{name: "assigned clone", agent: "agent-a", assignee: "agent-a-w25"},
		{name: "mismatch", agent: "agent-b", assignee: "agent-a", bad: true},
		{name: "missing", assignee: "agent-a", bad: true},
		{name: "must", agent: "agent-b", assignee: "agent-a", mode: "must", bad: true},
		{name: "precedence", agent: "agent-b", env: "agent-a", assignee: "agent-a", bad: true},
		{name: "person exact", agent: "Person A", assignee: "Person A"},
		{name: "person alias", agent: "person-alias", assignee: "Person A", bad: true},
		{name: "person case", agent: "person a", assignee: "Person A", bad: true},
		{name: "unassigned", agent: "agent-b"},
		{name: "grandfather", assignee: "agent-a", mode: "must", grandfather: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dir, message := commitAttributionFixture(t, tc.assignee, !tc.grandfather)
			t.Setenv("BASHY_AGENT", tc.env)
			t.Setenv("BASHY_SPRINT_ENFORCE", tc.mode)
			if tc.agent != "" {
				message += "Agent: " + tc.agent + "\n"
			}
			path := filepath.Join(t.TempDir(), "message")
			if err := os.WriteFile(path, []byte(message), 0600); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := commitAttributionRun(path)
			if (err != nil) != (tc.bad && tc.mode == "must") {
				t.Fatalf("err=%v stderr=%s", err, stderr)
			}
			if tc.bad {
				for _, want := range []string{"Story #110", tc.assignee, "Agent:"} {
					if !strings.Contains(stderr, want) {
						t.Errorf("missing %q: %s", want, stderr)
					}
				}
			} else if stderr != "" {
				t.Fatal(stderr)
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != message {
				t.Fatal("hook changed commit message")
			}
			events := findWeaveStory(q, 87).Thread
			if tc.bad && tc.mode != "must" {
				if len(events) != 1 || events[0].Kind != "bypass" || !strings.Contains(events[0].Body, "commit") {
					t.Fatalf("events=%+v", events)
				}
				wantActor := tc.agent
				if wantActor == "" {
					wantActor = tc.env
				}
				if wantActor == "" {
					wantActor = "unknown"
				}
				if events[0].Author != wantActor {
					t.Fatalf("actor=%q want %q", events[0].Author, wantActor)
				}
			} else if len(events) != 0 {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

func commitAttributionFixture(t *testing.T, assignee string, managed bool) (string, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	dir := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", dir)
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_AGENT", "")
	t.Setenv("BASHY_SPRINT_ENFORCE", "")
	root := filepath.Join(home, "fleet")
	t.Setenv("BASHY_FLEET_DIR", root)
	t.Setenv("BASHY_AGENTS_DIR", filepath.Join(root, "agents"))
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	for _, a := range []fleet.Agent{{Name: "agent-a", Tool: "tool-a", Model: "model-a"}, {Name: "agent-a-w25", Tool: "tool-a", Model: "model-a", ClonedFrom: "agent-a"}, {Name: "agent-b", Tool: "tool-b", Model: "model-b"}} {
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SavePerson(fleet.Person{Handle: "Person A", Aliases: []string{"person-alias"}}); err != nil {
		t.Fatal(err)
	}
	repo := weaveTestRepo(t)
	wd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.MkdirAll(filepath.Join(repo, "docs", "todo"), 0755); err != nil {
		t.Fatal(err)
	}
	story := fmt.Sprintf("---\nid: d1e86f29d7a7\nseq: 110\ntitle: Delivery\nstatus: todo\nsprint: 87\nassignee: %s\n---\n", assignee)
	if err := os.WriteFile(filepath.Join(repo, "docs", "todo", "d1e86f29d7a7-delivery.md"), []byte(story), 0644); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 87, StoryRoots: []string{repo}}
	if managed {
		s.Lease = &weaveStoryLease{TokenHash: "hash"}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}
	return repo, dir, "deliver\n\nSprint: #87\nStory: #110\nStory-ID: d1e86f29d7a7\n"
}

func commitAttributionRun(args ...string) (string, string, error) {
	cmd := newSprintCommitMsgCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

func TestCommitAttributionCheckRangeDedupes(t *testing.T) {
	repo, dir, message := commitAttributionFixture(t, "agent-a", true)
	t.Setenv("BASHY_AGENT", "agent-a") // must not attribute historical commits to the checker
	for i := 0; i < 2; i++ {
		weaveTestGit(t, repo, "commit", "--allow-empty", "-qm", fmt.Sprintf("change %d\n%s", i, message))
	}
	for i := 0; i < 2; i++ {
		out, stderr, err := commitAttributionRun("--check-range", "HEAD~2..HEAD")
		if err != nil {
			t.Fatalf("%v: %s", err, stderr)
		}
		if len(strings.Split(strings.TrimSpace(out), "\n")) != 2 {
			t.Fatalf("output=%q", out)
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(findWeaveStory(q, 87).Thread) != 2 {
			t.Fatalf("thread=%+v", findWeaveStory(q, 87).Thread)
		}
	}
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	if _, _, err := commitAttributionRun("--check-range", "HEAD~2..HEAD"); err == nil {
		t.Fatal("must accepted unattributed history")
	}
}

func TestCommitAttributionRangeExemptions(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			repo, dir, message := commitAttributionFixture(t, "agent-a", managed)
			if managed {
				message += "Agent: agent-a\n"
			}
			weaveTestGit(t, repo, "commit", "--allow-empty", "-qm", message)
			t.Setenv("BASHY_SPRINT_ENFORCE", "must")
			out, stderr, err := commitAttributionRun("--check-range", "HEAD~1..HEAD")
			if err != nil || out != "" || stderr != "" {
				t.Fatalf("err=%v out=%s stderr=%s", err, out, stderr)
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(findWeaveStory(q, 87).Thread) != 0 {
				t.Fatal("unexpected bypass")
			}
		})
	}
}

func TestCommitAttributionRangeMustRecordsBypass(t *testing.T) {
	repo, dir, message := commitAttributionFixture(t, "agent-a", true)
	weaveTestGit(t, repo, "commit", "--allow-empty", "-qm", message+"Agent: agent-b\n")
	sha := weaveTestGit(t, repo, "rev-parse", "HEAD")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	out, _, err := commitAttributionRun("--check-range", "HEAD~1..HEAD")
	if err == nil || !strings.Contains(out, sha) {
		t.Fatalf("err=%v out=%s", err, out)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	events := findWeaveStory(q, 87).Thread
	if len(events) != 1 || events[0].Author != "agent-b" || !strings.Contains(events[0].Body, sha) {
		t.Fatalf("events=%+v", events)
	}
}
