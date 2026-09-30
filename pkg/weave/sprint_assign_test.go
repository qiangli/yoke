package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/qiangli/yoke/pkg/fleet"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

func TestAssignDispatch(t *testing.T) {
	for _, tc := range []struct {
		name              string
		busy, dry, manual bool
		band              int
		want              string
		launches          int
	}{
		{name: "fit", band: 3, want: "match", launches: 1},
		{name: "busy", busy: true, band: 3, want: "wait"},
		{name: "cascade-one-down", band: 4, want: "cascade:L3", launches: 1},
		{name: "cascade-two-down-never-wait", band: 5, want: "cascade:L3", launches: 1},
		{name: "dry-run", dry: true, band: 3, want: "match"},
		{name: "manual", manual: true, band: 3, want: "manual override", launches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BASHY_HOME", home)
			t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
			t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
			pool := []ladder.Entrant{{Agent: "agent-a", Vendor: "tool-a", Band: 3, Free: !tc.busy, Standings: map[ladder.Duty]ladder.DutyStanding{ladder.DutyCode: {R: 1800, RD: 50}}}}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetContext(context.Background())
			launches, events := 0, 0
			deps := sprintAssignDeps{launch: func(_ *cobra.Command, r sprintAssignLaunch) error {
				launches++
				if r.Env["BASHY_AGENT"] != "agent-a" {
					t.Fatalf("missing identity: %+v", r)
				}
				return nil
			}, record: func(kind string, e sprintAssignEvent) error {
				events++
				if kind != "assign" || e.Run != 7 || e.Agent != "agent-a" {
					t.Fatalf("event: %s %+v", kind, e)
				}
				if tc.manual && !e.Manual {
					t.Fatal("override not logged")
				}
				return nil
			}, seed: func() (int64, error) { return 7, nil }}
			manual := ""
			if tc.manual {
				manual = "agent-a"
			}
			err := sprintAssignDispatch(cmd, ladder.StoryTask{ID: "story", Duty: ladder.DutyCode, Band: tc.band, Rating: 1550}, pool, ladder.Lines{L4Code: 1750}, manual, tc.dry, "repo", deps)
			if err != nil {
				t.Fatal(err)
			}
			if launches != tc.launches || events != tc.launches {
				t.Fatalf("launches=%d events=%d", launches, events)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatal(out.String())
			}
		})
	}
}

func TestReviewDispatchDominanceAndEscalation(t *testing.T) {
	author := ladder.DutyStanding{R: 1800, RD: 50}
	pool := []ladder.Entrant{{Agent: "agent-b", Vendor: "tool-b", Band: 4, Free: true, Standings: map[ladder.Duty]ladder.DutyStanding{ladder.DutyCode: {R: 1950, RD: 50}}}}
	for _, dry := range []bool{true, false} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		events := 0
		record := func(kind string, e sprintAssignEvent) error {
			events++
			if kind != "review-assign" || e.Agent != "agent-b" {
				t.Fatalf("%s %+v", kind, e)
			}
			return nil
		}
		if err := sprintReviewDispatch(cmd, "story", author, 3, "tool-a", pool, dry, record); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "agent-b") || events != map[bool]int{true: 0, false: 1}[dry] {
			t.Fatal(out.String(), events)
		}
	}
	pool[0].Standings[ladder.DutyCode] = ladder.DutyStanding{R: 1800, RD: 50}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := sprintReviewDispatch(cmd, "story", author, 3, "tool-a", pool, true, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "escalate to owner") {
		t.Fatal(out.String())
	}
}

func TestAssignBusyUsesLiveQueueCheck(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{ID: 4, State: "working", WrapperPid: os.Getpid(), LaunchSpec: &weaveLaunchSpec{Agent: "agent-a"}}}}
	if !sprintAssignBusy([]*weaveQueue{q}, []string{"agent-a"}) {
		t.Fatal("live run not excluded")
	}
	q.Items[0].State = "done"
	if sprintAssignBusy([]*weaveQueue{q}, []string{"agent-a"}) {
		t.Fatal("terminal run excluded")
	}
}

func TestAssignCommandSeedsLinksAndInjectsIdentity(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = old })
	for _, err := range []error{cat.SaveTool(fleet.Tool{Name: "tool-a"}), cat.SaveModel(fleet.Model{Name: "model-a", Band: 3, CostMicro: 100}), cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	board, _ := sprintStoreDir()
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{ID: 1, Title: "dispatch", Column: "doing"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	story, err := todopkg.Add(todopkg.RepoStore(root), "implement", "story body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err = todopkg.RepoStore(root).Save(story); err != nil {
		t.Fatal(err)
	}
	launched := 0
	oldLaunch := sprintAssignLaunchWorker
	t.Cleanup(func() { sprintAssignLaunchWorker = oldLaunch })
	sprintAssignLaunchWorker = func(_ *cobra.Command, r sprintAssignLaunch) error {
		launched++
		if r.Agent != "agent-a" || r.Env["BASHY_AGENT"] != "agent-a" || r.Repo != root {
			t.Fatalf("launch %+v", r)
		}
		dir, _ := weaveQueueDir(root)
		q, err := loadWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		run := findWeaveItem(q, r.Run)
		if run == nil || run.Points != 5 || run.Register != story.ID || !strings.Contains(run.Body, "story body") || !strings.Contains(run.Body, "Story-ID: "+story.ID) {
			t.Fatalf("run %+v", run)
		}
		b, err := loadWeaveQueue(board)
		if err != nil {
			t.Fatal(err)
		}
		s := findWeaveStory(b, 1)
		if len(s.Runs) != 1 || s.Runs[0].ID != r.Run {
			t.Fatalf("links %+v", s.Runs)
		}
		data, _ := json.Marshal(s)
		if !bytes.Contains(data, []byte("assign")) {
			t.Fatalf("missing assignment before launch: %s", data)
		}
		return nil
	}
	for _, tc := range []struct {
		name, env, reason string
		flags             []string
		shadow, probe     bool
	}{
		{name: "tool flags", flags: []string{"--exclude-tool", "tool-a", "--exclude-tool", "other"}, reason: "tool:tool-a"},
		{name: "agent flags", flags: []string{"--exclude-agent", "agent-a", "--exclude-agent", "other"}, reason: "agent:agent-a"},
		{name: "tool env", env: " tool:tool-a , agent:other ", reason: "tool:tool-a"},
		{name: "agent env", env: "agent:agent-a", reason: "agent:agent-a"},
		{name: "binding env", env: "agent:tool-a:model-a", reason: "agent:tool-a:model-a"},
		{name: "additive", env: "tool:tool-a", flags: []string{"--exclude-agent", "other"}, reason: "tool:tool-a"},
		{name: "shadow", shadow: true, reason: "shadow"},
		{name: "probe", probe: true, reason: "cached probe: unusable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASHY_SPRINT_DISPATCH_EXCLUDE", tc.env)
			if tc.shadow {
				if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3, Role: &fleet.AgentRole{Scope: "shadow"}}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3}); err != nil {
						t.Error(err)
					}
				})
			}
			if tc.probe {
				dir, err := weaveQueueDir(root)
				if err != nil {
					t.Fatal(err)
				}
				saveFleetProbeCache(dir, map[string]fleetProbeEntry{"tool-a": {Capable: false, ProbedAt: time.Now()}})
				t.Cleanup(func() { saveFleetProbeCache(dir, nil) })
			}
			for _, manual := range []bool{false, true} {
				cmd := newSprintAssignCmd()
				args := append([]string{"1", story.ID, "--repo", root, "--dry-run"}, tc.flags...)
				if manual {
					args = append(args, "--agent", "agent-a")
				}
				cmd.SetArgs(args)
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				err := cmd.Execute()
				if manual {
					if err == nil || !strings.Contains(err.Error(), "not an eligible fleet binding") {
						t.Fatalf("manual exclusion: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "excluded=agent-a") || !strings.Contains(out.String(), tc.reason) || (!manual && !strings.Contains(out.String(), "wait: no free entrant")) {
					t.Fatal(out.String())
				}
				if launched != 0 {
					t.Fatal("excluded entrant launched")
				}
			}
		})
	}
	for _, dry := range []bool{true, false} {
		cmd := newSprintAssignCmd()
		args := []string{"1", story.ID, "--repo", root, "--points", "5", "--agent", "agent-a"}
		if dry {
			args = append(args, "--dry-run")
		}
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if dry && launched != 0 {
			t.Fatal("dry run launched")
		}
	}
	if launched != 1 {
		t.Fatal(launched)
	}
	loaded, err := todopkg.RepoStore(root).Resolve(story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Assignee != "agent-a" || loaded.Weave == 0 {
		t.Fatalf("story %+v", loaded)
	}
	if _, err := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(err) {
		t.Fatal("dispatch must not write ladder events", err)
	}
}

func TestAssignPoolReplayProbeAndBusyClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = old })
	for _, err := range []error{cat.SaveTool(fleet.Tool{Name: "tool-a"}), cat.SaveModel(fleet.Model{Name: "model-a", Band: 3, CostMicro: 100}), cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3}), cat.SaveAgent(fleet.Agent{Name: "agent-a-copy", Tool: "tool-a", Model: "model-a", ClonedFrom: "agent-a", Ephemeral: true})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	now := time.Now()
	pool, _, err := sprintAssignPool(root, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].Band != 3 || !pool[0].Free || pool[0].CostPerPoint != 100 {
		t.Fatalf("pool %+v", pool)
	}
	store, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Append(ladder.Event{Kind: ladder.EventKindSeat, Agent: "tool-a:model-a", At: now, Season: ladder.SeasonOf(now), Provisional: 5}); err != nil {
		t.Fatal(err)
	}
	events, err := sprintAssignReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	pool, _, err = sprintAssignPool(root, events, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].Band != 5 {
		t.Fatalf("provisional pool %+v", pool)
	}
	dir, _ := weaveQueueDir(root)
	if err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Items = append(q.Items, &weaveItem{ID: 1, State: "working", WrapperPid: os.Getpid(), LaunchSpec: &weaveLaunchSpec{Agent: "agent-a-copy"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pool, _, err = sprintAssignPool(root, events, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].Free {
		t.Fatalf("busy clone %+v", pool)
	}
	saveFleetProbeCache(dir, map[string]fleetProbeEntry{"tool-a": {Capable: false, ProbedAt: now}})
	pool, _, err = sprintAssignPool(root, events, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 0 {
		t.Fatalf("unusable tool %+v", pool)
	}
}

func TestReviewCommandUnknownStandingEscalates(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = old })
	board, _ := sprintStoreDir()
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{ID: 1, Title: "review"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	story, err := todopkg.Add(todopkg.RepoStore(root), "task", "body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err = todopkg.RepoStore(root).Save(story); err != nil {
		t.Fatal(err)
	}
	cmd := newSprintReviewCmd()
	cmd.SetArgs([]string{"1", story.ID, "--author", "tool-a:model-a", "--author-band", "3"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err = cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "escalate to owner") {
		t.Fatal(out.String())
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(findWeaveStory(q, 1))
	if !bytes.Contains(data, []byte("review-assign")) {
		t.Fatalf("missing escalation event: %s", data)
	}
}
