package weave

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func TestReassignDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, state, evidence  string
		dry, environment       bool
		kills, seeds, launches int
	}{
		{"killed at cap", "killed", "max-runtime", false, false, 0, 1, 1},
		{"running past cap", "working", "", false, false, 1, 1, 1},
		{"environment timeout", "killed", "rate limit exceeded", false, true, 0, 1, 1},
		{"dry run", "working", "", true, false, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetOut(&bytes.Buffer{})
			now := time.Now().UTC()
			run := &weaveItem{ID: 1, State: tc.state, Points: 1, Register: "story-a", StartedAt: now.Add(-6 * time.Minute), KilledBy: tc.evidence, Owner: "agent-a", Branch: "keep-me"}
			if tc.environment {
				run.Completion = "max-runtime"
			}
			pool := []ladder.Entrant{{Agent: "agent-a", Band: 3, Free: true}, {Agent: "agent-b", Band: 3, Free: true}}
			var kills, seeds, launches int
			var got sprintReassignEvent
			deps := sprintReassignDeps{
				kill: func() error { kills++; return nil },
				seed: func() (int64, error) { seeds++; return 2, nil },
				launch: func(agent string, id int64, cap time.Duration) error {
					launches++
					if agent != "agent-b" || id != 2 || cap != 5*time.Minute {
						t.Fatalf("launch %s %d %s", agent, id, cap)
					}
					return nil
				},
				record: func(e sprintReassignEvent) error { got = e; return nil },
			}
			if err := sprintReassignDispatch(cmd, run, "agent-a", map[string]bool{"agent-a": true}, pool, tc.dry, tc.environment, now, deps); err != nil {
				t.Fatal(err)
			}
			if kills != tc.kills || seeds != tc.seeds || launches != tc.launches {
				t.Fatalf("effects %d/%d/%d", kills, seeds, launches)
			}
			if !tc.dry && (got.FromRun != 1 || got.ToRun != 2 || got.ToAgent != "agent-b" || run.Branch != "keep-me") {
				t.Fatalf("event=%+v branch=%s", got, run.Branch)
			}
		})
	}
}

func TestReassignOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		accepted, environment bool
		wantA, wantB          float64
	}{
		{"B success", true, false, 0, 1},
		{"B failure", false, false, 0, 0},
		{"environment A", true, true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := []sprintReassignEvent{{Story: "story-a", FromRun: 1, FromAgent: "agent-a", ToRun: 2, ToAgent: "agent-b", Points: 1, Environment: tc.environment}}
			events := sprintReassignOutcomes(chain, tc.accepted, blame.Attribution{Class: blame.ClassAgent, Evidence: []blame.Evidence{{Kind: blame.EvidenceGate, Ref: "failed"}}, By: "manager", At: time.Now()}, time.Now())
			if len(events) != 2 || events[0].Outcome != tc.wantA || events[1].Outcome != tc.wantB || events[0].Note != "reassign:story-a" || events[1].Note != "reassign:story-a" {
				t.Fatalf("events=%+v", events)
			}
			if tc.environment && events[0].Blame.Class != blame.ClassEnvironment {
				t.Fatalf("A blame=%+v", events[0].Blame)
			}
		})
	}
}

func TestReassignUsesLowerBandWhenRequiredSeatUnavailable(t *testing.T) {
	now := time.Now().UTC()
	run := &weaveItem{ID: 1, Register: "story-a", State: "killed", KilledBy: "max-runtime", Points: 1, Band: 3}
	pool := []ladder.Entrant{{Agent: "agent-a", Band: 3, Free: false}, {Agent: "agent-b", Band: 2, Free: true}}
	var recorded sprintReassignEvent
	deps := sprintReassignDeps{seed: func() (int64, error) { return 2, nil }, record: func(e sprintReassignEvent) error { recorded = e; return nil }, launch: func(string, int64, time.Duration) error { return nil }}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := sprintReassignDispatch(cmd, run, "agent-a", map[string]bool{"agent-a": true}, pool, false, false, now, deps); err != nil {
		t.Fatal(err)
	}
	if recorded.WantedBand != 3 || recorded.Band != 2 {
		t.Fatalf("band fallback=%+v", recorded)
	}
}

func TestReassignAcceptFailChain(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		name := "fail"
		if accepted {
			name = "accept"
		}
		t.Run(name, func(t *testing.T) {
			home, repo := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
			t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
			t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
			t.Setenv("BASHY_PRINCIPAL", "")
			t.Setenv("WEAVE_CONDUCTOR", "manager")
			st := todopkg.RepoStore(repo)
			story, err := todopkg.Add(st, "deliver", "", "p0", nil, "", "")
			if err != nil {
				t.Fatal(err)
			}
			story.Sprint, story.Weave, story.Assignee, story.Status = 1, 2, "agent-b", todopkg.StatusAssigned
			if _, err := st.Save(story); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			qdir, err := weaveQueueDir(repo)
			if err != nil {
				t.Fatal(err)
			}
			tag := filepath.Base(qdir)
			a := &weaveItem{ID: 1, Register: story.ID, Points: 1, State: "killed", KilledBy: "max-runtime", Owner: "agent-a", Created: now.Add(-10 * time.Minute), StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-5 * time.Minute), Branch: "keep-a"}
			b := &weaveItem{ID: 2, Register: story.ID, Points: 1, State: "submitted", Owner: "agent-b", Created: now.Add(-4 * time.Minute), StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-time.Minute)}
			if err := saveWeaveQueue(qdir, &weaveQueue{Root: repo, Items: []*weaveItem{a, b}}); err != nil {
				t.Fatal(err)
			}
			s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: now}, StoryRoots: []string{repo}, Created: now, Runs: []sprintRun{{Repo: filepath.Base(repo), Queue: tag, ID: 1, Born: a.Created}, {Repo: filepath.Base(repo), Queue: tag, ID: 2, Born: b.Created}}}
			e := sprintReassignEvent{Story: story.ID, FromRun: 1, FromAgent: "agent-a", ToRun: 2, ToAgent: "agent-b", Points: 1, Cap: "5m0s"}
			body, _ := json.Marshal(e)
			weaveStoryAppend(s, "manager", "reassign", string(body))
			weaveStoryAppend(s, "agent-b", "decision", "agent-b submitted story "+shortSprintStoryID(story.ID)+" for merge/closure")
			if err := saveWeaveQueue(filepath.Join(home, "sprint"), &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
				t.Fatal(err)
			}
			cmd := NewSprintCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			args := []string{name, "1", story.ID, "--repo", repo}
			if !accepted {
				args = append(args, "--blame", "agent", "--evidence", "gate:failed")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
			store, err := ladder.OpenStore("")
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[0].Agent != "agent-a" || events[0].Blame.Class != blame.ClassAgent || events[1].Agent != "agent-b" || events[1].Outcome != map[bool]float64{true: 1, false: 0}[accepted] {
				t.Fatalf("events=%+v", events)
			}
			got, err := todopkg.ResolveRef(st, story.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !accepted && (got.Status != todopkg.StatusTodo || !containsReassignLabel(got.Labels, "split-needed")) {
				t.Fatalf("story=%+v", got)
			}
			if accepted && got.Status != todopkg.StatusDone {
				t.Fatalf("story=%+v", got)
			}
			queue, err := loadWeaveQueue(qdir)
			if err != nil || queue.Items[0].Branch != "keep-a" {
				t.Fatalf("branch lost: %v %+v", err, queue)
			}
		})
	}
}

func containsReassignLabel(labels []string, label string) bool {
	for _, x := range labels {
		if x == label {
			return true
		}
	}
	return false
}

func TestReassignEnvironmentDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	if err := os.WriteFile(path, []byte("provider returned rate limit exceeded"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &weaveItem{LogPath: path}
	if yes, marker := sprintReassignEnvironment(run); !yes || marker != "rate limit" {
		t.Fatalf("%t %q", yes, marker)
	}
}

func TestReassignCommandSeedsFreshLinkedRun(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dry, environment bool
	}{
		{"dry", true, false}, {"live", false, false}, {"environment", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repo := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BASHY_HOME", home)
			t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
			t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
			t.Setenv("BASHY_PRINCIPAL", "")
			t.Setenv("WEAVE_CONDUCTOR", "manager")
			t.Setenv("BASHY_SPRINT_ENFORCE", "")
			cat := pinFleetWith(t)
			for _, err := range []error{
				cat.SaveTool(fleet.Tool{Name: "tool-a"}), cat.SaveTool(fleet.Tool{Name: "tool-b"}),
				cat.SaveModel(fleet.Model{Name: "model-a", Band: 3}), cat.SaveModel(fleet.Model{Name: "model-b", Band: 3}),
				cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3}),
				cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "model-b", Band: 3}),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			st := todopkg.RepoStore(repo)
			story, err := todopkg.Add(st, "deliver", "original body", "p0", nil, "", "")
			if err != nil {
				t.Fatal(err)
			}
			story.Sprint, story.Weave, story.Assignee, story.Status = 1, 1, "agent-a", todopkg.StatusAssigned
			if _, err := st.Save(story); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			run := &weaveItem{ID: 1, Register: story.ID, Title: story.Title, Body: "exact old prompt", Points: 1, Band: 3, State: "killed", KilledBy: "max-runtime", Owner: "agent-a", Created: now.Add(-6 * time.Minute), StartedAt: now.Add(-6 * time.Minute), FinishedAt: now, Branch: "keep-a", LaunchSpec: &weaveLaunchSpec{Agent: "agent-a"}}
			if tc.environment {
				run.VerifyOutput = "rate limit exceeded"
			}
			qdir, err := weaveQueueDir(repo)
			if err != nil {
				t.Fatal(err)
			}
			tag := filepath.Base(qdir)
			if err := saveWeaveQueue(qdir, &weaveQueue{Root: repo, NextID: 2, Items: []*weaveItem{run}}); err != nil {
				t.Fatal(err)
			}
			s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: now}, StoryRoots: []string{repo}, Created: now, Runs: []sprintRun{{Repo: filepath.Base(repo), Queue: tag, ID: 1, Born: run.Created}}}
			if err := saveWeaveQueue(filepath.Join(home, "sprint"), &weaveQueue{Stories: []*weaveStory{s}}); err != nil {
				t.Fatal(err)
			}
			priorPool, priorLaunch := sprintReassignSeatPool, sprintReassignLaunchRun
			sprintReassignSeatPool = func(_ string, _ []ladder.Event, _ time.Time, _ ...sprintAssignExclusions) ([]ladder.Entrant, ladder.Lines, error) {
				return []ladder.Entrant{{Agent: "agent-a", Band: 3, Free: true}, {Agent: "agent-b", Band: 3, Free: true}}, ladder.Lines{}, nil
			}
			launches := 0
			sprintReassignLaunchRun = func(_ *cobra.Command, root, agent string, id int64, cap time.Duration) error {
				launches++
				if root != repo || agent != "agent-b" || id != 2 || cap != 5*time.Minute {
					t.Fatalf("launch %s %s %d %s", root, agent, id, cap)
				}
				return nil
			}
			t.Cleanup(func() { sprintReassignSeatPool = priorPool; sprintReassignLaunchRun = priorLaunch })
			cmd := NewSprintCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			args := []string{"reassign", "1", "--run", filepath.Base(repo) + "#1"}
			if tc.dry {
				args = append(args, "--dry-run")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
			q, err := loadWeaveQueue(qdir)
			if err != nil {
				t.Fatal(err)
			}
			board, err := loadWeaveQueue(filepath.Join(home, "sprint"))
			if err != nil {
				t.Fatal(err)
			}
			if q.Items[0].Branch != "keep-a" {
				t.Fatal("branch lost")
			}
			if tc.dry {
				if launches != 0 || len(q.Items) != 1 || len(board.Stories[0].Runs) != 1 {
					t.Fatalf("dry mutated: %d %+v %+v", launches, q.Items, board.Stories[0].Runs)
				}
			} else {
				if launches != 1 || len(q.Items) != 2 || q.Items[1].Body != "exact old prompt" || len(board.Stories[0].Runs) != 2 {
					t.Fatalf("reassign: %d %+v %+v", launches, q.Items, board.Stories[0].Runs)
				}
				chain := sprintReassignChain(board.Stories[0], story.ID)
				if len(chain) != 1 || chain[0].FromAgent != "tool-a:model-a" || chain[0].ToAgent != "tool-b:model-b" {
					t.Fatalf("chain=%+v", chain)
				}
				if tc.environment {
					store, err := ladder.OpenStore("")
					if err != nil {
						t.Fatal(err)
					}
					events, err := store.Read()
					if err != nil {
						t.Fatal(err)
					}
					if len(events) != 1 || events[0].Blame.Class != blame.ClassEnvironment || blame.Consequence(events[0].Blame) != blame.ActionOpenFix || events[0].Note != "reassign:"+story.ID {
						t.Fatalf("events=%+v", events)
					}
					if _, failures := seatRecord(events, "tool-a:model-a"); failures != 0 {
						t.Fatalf("environment timeout charged agent: %+v", events)
					}
				}
			}
		})
	}
}

func TestReassignSecondTimeoutMarksSplit(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	t.Setenv("BASHY_SPRINT_ENFORCE", "")
	st := todopkg.RepoStore(repo)
	story, err := todopkg.Add(st, "deliver", "body", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint, story.Weave, story.Assignee, story.Status = 1, 2, "agent-b", todopkg.StatusAssigned
	if _, err := st.Save(story); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	qdir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	a := &weaveItem{ID: 1, Register: story.ID, Points: 1, State: "killed", KilledBy: "max-runtime", Owner: "agent-a", Created: now.Add(-10 * time.Minute)}
	b := &weaveItem{ID: 2, Register: story.ID, Points: 1, State: "killed", KilledBy: "max-runtime", Owner: "agent-b", Created: now.Add(-5 * time.Minute), Branch: "keep-b"}
	if err := saveWeaveQueue(qdir, &weaveQueue{Root: repo, NextID: 3, Items: []*weaveItem{a, b}}); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: now}, StoryRoots: []string{repo}, Created: now, Runs: []sprintRun{{Repo: filepath.Base(repo), Queue: filepath.Base(qdir), ID: 1, Born: a.Created}, {Repo: filepath.Base(repo), Queue: filepath.Base(qdir), ID: 2, Born: b.Created}}}
	e := sprintReassignEvent{Story: story.ID, Repo: filepath.Base(repo), FromRun: 1, FromAgent: "tool-a:model-a", ToRun: 2, ToAgent: "tool-b:model-b", Points: 1, Cap: "5m0s"}
	body, _ := json.Marshal(e)
	weaveStoryAppend(s, "manager", "reassign", string(body))
	if err := saveWeaveQueue(filepath.Join(home, "sprint"), &weaveQueue{Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}
	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"reassign", "1", "--run", filepath.Base(repo) + "#2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	got, err := todopkg.ResolveRef(st, story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsReassignLabel(got.Labels, "split-needed") || got.Status != todopkg.StatusTodo {
		t.Fatalf("story=%+v", got)
	}
	board, err := loadWeaveQueue(filepath.Join(home, "sprint"))
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Stories[0].Runs) != 2 || board.Stories[0].Thread[len(board.Stories[0].Thread)-1].Kind != "split-needed" {
		t.Fatalf("board=%+v", board.Stories[0])
	}
	store, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Blame.Evidence[0].Ref != "timeout:1" || events[1].Blame.Evidence[0].Ref != "timeout:2" {
		t.Fatalf("events=%+v", events)
	}
	cmd = NewSprintCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"reassign", "1", "--run", filepath.Base(repo) + "#2"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("split story was reassigned again")
	}
}
