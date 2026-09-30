package weave

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestSprintLadderLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, verb      string
		extra           []string
		wall            time.Duration
		outcome         float64
		count           int
		bad, storeError bool
	}{
		{"explicit", "accept", []string{"--agent", "tool-b:model-b", "--points", "2"},
			time.Minute, 1, 1, false, false},
		{"no-run", "accept", nil, time.Minute, 0, 0, false, false},
		{"unknown-accept", "accept", nil, time.Minute, 0, 0, false, false},
		{"unknown-fail", "fail", nil, time.Minute, 0, 0, true, false},
		{"unknown-override", "accept", []string{"--agent", "tool-b:model-b", "--points", "2"}, time.Minute, 1, 1, false, false},
		{"within", "accept", nil, time.Minute, 1, 1, false, false},
		{"turns", "accept", nil, time.Minute, .5, 1, false, false},
		{"story-link", "accept", nil, time.Minute, 1, 1, false, false},
		{"over", "accept", nil, 16 * time.Minute, .5, 1, false, false},
		{"rework", "accept", []string{"--rework", "1"}, time.Minute, .5, 1, false, false},
		{"skip", "accept", []string{"--no-rating"}, time.Minute, 0, 0, false, false},
		{"store", "accept", nil, time.Minute, 0, 0, false, true},
		{"agent", "fail", []string{"--blame", "agent", "--evidence", "gate:check-1"}, time.Minute, 0, 1, false, false},
		{"invalid", "fail", []string{"--blame", "agent", "--evidence", "quota:q1"}, time.Minute, 0, 0, true, false},
		{"lease", "fail", []string{"--blame", "agent", "--evidence", "gate:g1"}, time.Minute, 0, 0, true, false},
		{"missing", "fail", []string{"--blame", "agent"}, time.Minute, 0, 0, true, false},
		{"unclassified", "fail", nil, time.Minute, 0, 1, false, false},
		{"environment", "fail", []string{"--blame", "environment", "--evidence", "host:check-1"}, time.Minute, 0, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repo := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
			cat := pinFleetWith(t)
			if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a"}); err != nil {
				t.Fatal(err)
			}
			clone, err := cat.CloneAgent("agent-a", "agent-a-w25", false, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveAgent(clone); err != nil {
				t.Fatal(err)
			}
			board := filepath.Join(home, "sprint")
			t.Setenv("BASHY_SPRINT_DIR", board)
			t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
			t.Setenv("BASHY_PRINCIPAL", "")
			t.Setenv("WEAVE_CONDUCTOR", "manager")
			t.Setenv("BASHY_LADDER_SEASON", "4")
			st := todopkg.RepoStore(repo)
			it, err := todopkg.Add(st, "deliver", "", "p0", nil, "", "")
			if err != nil {
				t.Fatal(err)
			}
			it.Sprint = 1
			it.Assignee = "agent-a-w25"
			it.Status = todopkg.StatusAssigned
			it.Weave = 25
			if _, err = st.Save(it); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			run := &weaveItem{ID: 25, Register: it.ID, Points: 1, Owner: "agent-a-w25", Tool: "tool-a", Created: now, StartedAt: now.Add(-tc.wall), FinishedAt: now, LaunchSpec: &weaveLaunchSpec{Tool: filepath.Join(home, "bin", "tool-a"), Model: "wire-model-a-v1", Agent: "agent-a-w25"}}
			if strings.HasPrefix(tc.name, "unknown-") {
				run.LaunchSpec.Agent = "unknown-agent-w25"
			}
			if tc.name == "turns" {
				run.LogPath = filepath.Join(home, "result.jsonl")
				if err = os.WriteFile(run.LogPath, []byte("{\"type\":\"result\",\"num_turns\":21}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "story-link" {
				run.Register = ""
			}
			tag := filepath.Base(repo) + "-test"
			if err = saveWeaveQueue(filepath.Join(weaveStateRoot(home), tag), &weaveQueue{Root: repo, Items: []*weaveItem{run}}); err != nil {
				t.Fatal(err)
			}
			s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: now}, StoryRoots: []string{repo}, Created: now, Runs: []sprintRun{{Repo: filepath.Base(repo), Queue: tag, ID: 25, Born: now}}}
			if tc.name == "explicit" || tc.name == "no-run" {
				s.Runs = nil
				it.Weave = 0
				if _, err = st.Save(it); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "lease" {
				s.Lease.TokenHash = sprintLeaseTokenHash("required")
				t.Setenv("BASHY_SPRINT_ENFORCE", "must")
				t.Setenv(sprintLeaseTokenEnv, "")
			}
			weaveStoryAppend(s, "agent-a-w25", "decision", "agent-a-w25 submitted story "+shortSprintStoryID(it.ID)+" for merge/closure")
			if err = saveWeaveQueue(board, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
				t.Fatal(err)
			}
			if tc.storeError {
				if err = os.MkdirAll(ladder.DefaultStorePath(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := NewSprintCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{tc.verb, "1", it.ID, "--repo", repo}, tc.extra...))
			err = cmd.Execute()
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v out=%s stderr=%s", err, out.String(), stderr.String())
			}
			got, _ := todopkg.ResolveRef(st, it.ID)
			if tc.bad {
				if got.Status != todopkg.StatusAssigned {
					t.Fatal("mutated invalid failure")
				}
			} else if tc.verb == "accept" {
				if got.Status != todopkg.StatusDone {
					t.Fatal(got.Status)
				}
			} else {
				if got.Status != todopkg.StatusTodo || got.Assignee != "" {
					t.Fatal("failure did not yield")
				}
				q, e := loadWeaveQueue(board)
				if e != nil {
					t.Fatal(e)
				}
				if q.Stories[0].Thread[len(q.Stories[0].Thread)-1].Kind != "fail" {
					t.Fatal("missing fail thread")
				}
			}
			if tc.storeError {
				if !strings.Contains(stderr.String(), "ladder:") {
					t.Fatal("missing store warning")
				}
				return
			}
			if tc.name == "skip" && !strings.Contains(out.String(), "unrated by manager") {
				t.Fatal(out.String())
			}
			if tc.name == "no-run" {
				want := "ladder: not rated — story has no linked run; pass --agent and --points to rate"
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing no-run notice: stdout=%s stderr=%s", out.String(), stderr.String())
				}
			}
			if tc.name == "unknown-accept" {
				want := "ladder: not rated — no canonical agent identity for the linked run; pass --agent tool:model"
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing identity notice: stdout=%s stderr=%s", out.String(), stderr.String())
				}
			}
			if tc.name == "unknown-fail" && (err == nil || !strings.Contains(stderr.String(), "no canonical agent identity")) {
				t.Fatalf("missing identity refusal: err=%v stderr=%s", err, stderr.String())
			}
			if tc.count == 0 {
				if _, e := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(e) {
					t.Fatalf("unexpected ledger: %v", e)
				}
				return
			}
			store, e := ladder.OpenStore("")
			if e != nil {
				t.Fatal(e)
			}
			events, e := store.Read()
			if e != nil {
				t.Fatal(e)
			}
			if len(events) != 1 {
				t.Fatal(events)
			}
			raw, e := os.ReadFile(ladder.DefaultStorePath())
			if e != nil {
				t.Fatal(e)
			}
			var fields map[string]any
			if e = json.Unmarshal(bytes.TrimSpace(raw), &fields); e != nil {
				t.Fatal(e)
			}
			for _, field := range []string{"agent", "story", "note"} {
				value, _ := fields[field].(string)
				if strings.HasPrefix(value, "/") || strings.Contains(value, home) || strings.Contains(value, "~/") || strings.Contains(value, "/Users/") || strings.Contains(value, "/home/") {
					t.Fatalf("path leaked into %s: %s", field, raw)
				}
			}
			ev := events[0]
			wantAgent, wantPoints, wantWall := "tool-a:model-a", ladder.Points(1), int(tc.wall.Seconds())
			if tc.name == "explicit" || tc.name == "unknown-override" {
				wantAgent = "tool-b:model-b"
				wantPoints = 2
				if tc.name == "explicit" {
					wantWall = 0
				}
			}
			if ev.Agent != wantAgent || ev.Points != wantPoints || ev.Outcome != tc.outcome || ev.Season != 4 || ev.Story != it.ID || ev.Sprint != 1 || ev.Reviewer != "manager" || ev.CapsUsed.WallSeconds != wantWall {
				t.Fatalf("%+v", ev)
			}
			if tc.name == "turns" && ev.CapsUsed.Turns != 21 {
				t.Fatal(ev.CapsUsed)
			}
			if tc.name == "agent" && (!blame.Rates(ev.Blame) || ev.Blame.By != "manager") {
				t.Fatal(ev.Blame)
			}
			if tc.name == "unclassified" && (blame.Rates(ev.Blame) || ladder.Replay(events, 4).Agents[ev.Agent].Unrated != 1) {
				t.Fatal("rated unclassified")
			}
			if tc.name == "environment" && !strings.Contains(out.String(), "open a fix item: bashy todo add") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestENOSPCGateFailureIsClassifiedEnvironment(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	cat := pinFleetWith(t)
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	board := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", board)
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	t.Setenv("BASHY_LADDER_SEASON", "4")
	st := todopkg.RepoStore(repo)
	story, err := todopkg.Add(st, "deliver", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	story.Assignee = "agent-a"
	story.Status = todopkg.StatusAssigned
	story.Weave = 25
	if _, err = st.Save(story); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := &weaveItem{
		ID:           25,
		Register:     story.ID,
		Points:       1,
		Owner:        "agent-a",
		Tool:         "tool-a",
		State:        "failed",
		Created:      now,
		StartedAt:    now.Add(-time.Minute),
		FinishedAt:   now,
		VerifyOutput: "go test ./pkg/weave: write /tmp/x: no space left on device",
		LaunchSpec:   &weaveLaunchSpec{Tool: filepath.Join(home, "bin", "tool-a"), Model: "model-a", Agent: "agent-a"},
	}
	tag := filepath.Base(repo) + "-test"
	if err = saveWeaveQueue(filepath.Join(weaveStateRoot(home), tag), &weaveQueue{Root: repo, Items: []*weaveItem{run}}); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: now}, StoryRoots: []string{repo}, Created: now, Runs: []sprintRun{{Repo: filepath.Base(repo), Queue: tag, ID: 25, Born: now}}}
	if err = saveWeaveQueue(board, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}

	cmd := NewSprintCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"fail", "1", story.ID, "--repo", repo})
	if err = cmd.Execute(); err != nil {
		t.Fatalf("sprint fail: %v stdout=%s stderr=%s", err, out.String(), stderr.String())
	}
	store, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	if events[0].Blame.Class != blame.ClassEnvironment {
		t.Fatalf("ENOSPC failure blame = %+v, want environment", events[0].Blame)
	}
	if !strings.Contains(out.String(), "Fix the delivery environment") {
		t.Fatalf("environment failure did not print fix-item prompt: stdout=%s stderr=%s", out.String(), stderr.String())
	}
}

func TestSprintLadderStreakBandAudit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "model-a", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	store, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	for i := 0; i < 5; i++ {
		ev := &ladder.Event{Kind: ladder.EventKindDelivery, Agent: "tool-a:model-a", Points: 1, Outcome: 1, Season: 1, At: time.Unix(int64(i+1), 0)}
		sprintLadderAppend(cmd, ev)
		events, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		want := i + 1
		if i == 4 {
			want++
		}
		if len(events) != want {
			t.Fatalf("delivery %d: events=%+v", i+1, events)
		}
		if i == 4 {
			move := events[len(events)-1]
			if move.Kind != ladder.EventKindBand || move.FromBand != 3 || move.ToBand != 4 || move.Note != "promote" {
				t.Fatalf("move=%+v", move)
			}
			if !strings.Contains(out.String(), "promoted L3 -> L4 (5 consecutive successes)") {
				t.Fatal(out.String())
			}
		}
	}
}
