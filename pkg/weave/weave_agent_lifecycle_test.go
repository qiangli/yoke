package weave

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/room"
)

func lifecycleHome(t *testing.T) (*fleet.Catalog, string, string) {
	t.Helper()
	fleettest.Ring(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_FLEET_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", "")
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_MINT_SPRINT", "")
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New() }
	t.Cleanup(func() { fleetCatalog = old })
	cat := fleetCatalog()
	if err := cat.SaveAgent(fleet.Agent{Name: "lifecycle-parent", Tool: "codex", Model: "gpt5.6-sol"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".bashy", "weave", "scratch")
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	return cat, dir, board
}

func assertArchived(t *testing.T, cat *fleet.Catalog, name string) {
	t.Helper()
	if _, ok := cat.Agent(name); ok {
		t.Fatalf("%s remains in catalog", name)
	}
	paths, err := filepath.Glob(filepath.Join(cat.Root(), "agents", "archive", name+"-*.yaml"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("archive %s: %v %v", name, paths, err)
	}
	body, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("lifecycle:")) || !bytes.Contains(body, []byte(name)) {
		t.Fatalf("archive lacks definition: %s", body)
	}
	cmd := fleet.NewAgentsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list", "--all", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"name":"`+name+`"`) || strings.Contains(out.String(), `"name": "`+name+`"`) {
		t.Fatalf("archived worker listed: %s", out.String())
	}
}

func TestEphemeralRunLifecycle(t *testing.T) {
	for _, state := range []string{"done", "abandoned"} {
		t.Run(state, func(t *testing.T) {
			cat, dir, _ := lifecycleHome(t)
			name, err := weaveCloneAgentForIssue("lifecycle-parent", 7, dir)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := cat.Agent(name)
			if !a.Ephemeral || a.Lifecycle == nil || a.Lifecycle.RunID != 7 || a.Lifecycle.WeaveQueue != dir {
				t.Fatalf("missing mint provenance: %+v", a)
			}
			q := &weaveQueue{Items: []*weaveItem{item(7, "submitted", name, 0)}}
			if err := saveWeaveQueue(dir, q); err != nil {
				t.Fatal(err)
			}
			if _, ok := cat.Agent(name); !ok {
				t.Fatal("submitted worker removed before merge")
			}
			q.Items[0].State = state
			if err := saveWeaveQueue(dir, q); err != nil {
				t.Fatal(err)
			}
			assertArchived(t, cat, name)
			stored, err := readWeaveQueue(dir)
			if err != nil || stored.Items[0].LaunchSpec.Agent != name {
				t.Fatalf("run evidence lost: %v %v", stored, err)
			}
			if _, ok := cat.Agent("lifecycle-parent"); !ok {
				t.Fatal("permanent parent removed")
			}
			if err := saveWeaveQueue(dir, q); err != nil {
				t.Fatal(err)
			}
			assertArchived(t, cat, name) // retry is idempotent
		})
	}
}

func TestEphemeralLifecycleProtectsBusyAgents(t *testing.T) {
	for _, protection := range []string{"wrapper", "wrapper-without-launch-spec", "finalizer", "reservation", "lease", "orchestrator-lease", "room", "other-run"} {
		t.Run(protection, func(t *testing.T) {
			cat, dir, board := lifecycleHome(t)
			name, err := weaveCloneAgentForIssue("lifecycle-parent", 7, dir)
			if err != nil {
				t.Fatal(err)
			}
			q := &weaveQueue{Items: []*weaveItem{item(7, "done", name, 0)}}
			switch protection {
			case "wrapper":
				q.Items[0].WrapperPid = os.Getpid()
			case "wrapper-without-launch-spec":
				q.Items[0].WrapperPid = os.Getpid()
				q.Items[0].LaunchSpec = nil
			case "finalizer":
				q.Items[0].FinalizerPID = os.Getpid()
			case "reservation":
				q.Items[0].ResourceReservationID = "held"
			case "lease":
				// Even an old lease must be explicitly released before removal.
				if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{{ID: 2, Lease: &weaveStoryLease{Holder: name, At: time.Now().Add(-24 * time.Hour)}}}}); err != nil {
					t.Fatal(err)
				}
			case "orchestrator-lease":
				if err := saveWeaveAutopilotLease(dir, weaveOrchestratorLease{Holder: name, Agent: name}); err != nil {
					t.Fatal(err)
				}
			case "room":
				if err := room.Join(room.Card{ID: name, Nick: name, PID: os.Getpid()}); err != nil {
					t.Fatal(err)
				}
			case "other-run":
				other := filepath.Join(filepath.Dir(dir), "other")
				if err := saveWeaveQueue(other, &weaveQueue{Items: []*weaveItem{item(9, "working", name, 0)}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveWeaveQueue(dir, q); err != nil {
				t.Fatal(err)
			}
			if _, ok := cat.Agent(name); !ok {
				t.Fatalf("removed agent protected by %s", protection)
			}
		})
	}
}

func TestEphemeralSprintMintAndEnd(t *testing.T) {
	cat, _, board := lifecycleHome(t)
	t.Setenv("BASHY_MINT_SPRINT", "sprint-uuid")
	for _, args := range [][]string{
		{"clone", "lifecycle-parent", "sprint-worker", "--fresh"},
		{"add", "sprint-seat", "--tool", "codex", "--model", "gpt5.6-sol"},
	} {
		cmd := fleet.NewAgentsCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sprint-worker", "sprint-seat"} {
		a, ok := cat.Agent(name)
		if !ok || !a.Ephemeral || a.Lifecycle == nil || a.Lifecycle.SprintID != "sprint-uuid" {
			t.Fatalf("%s missing sprint mint provenance: %+v", name, a)
		}
	}
	q := &weaveQueue{Stories: []*weaveStory{{ID: 1, UUID: "sprint-uuid", Column: "done", Lease: &weaveStoryLease{Holder: "sprint-seat"}}}}
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
	assertArchived(t, cat, "sprint-worker")
	if _, ok := cat.Agent("sprint-seat"); !ok {
		t.Fatal("leased seat removed")
	}
	q.Stories[0].Lease = nil
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
	assertArchived(t, cat, "sprint-seat")
}

func TestEphemeralRunReconcilesAfterLeaseRelease(t *testing.T) {
	cat, dir, board := lifecycleHome(t)
	name, err := weaveCloneAgentForIssue("lifecycle-parent", 7, dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &weaveQueue{Stories: []*weaveStory{{ID: 2, Lease: &weaveStoryLease{Holder: name}}}}
	if err := saveWeaveQueue(board, b); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{item(7, "abandoned", name, 0)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Agent(name); !ok {
		t.Fatal("leased agent removed")
	}
	b.Stories[0].Lease = nil
	if err := saveWeaveQueue(board, b); err != nil {
		t.Fatal(err)
	}
	assertArchived(t, cat, name)
}

func TestEphemeralLifecycleFailsClosed(t *testing.T) {
	for _, failure := range []string{"archive", "board", "room"} {
		t.Run(failure, func(t *testing.T) {
			cat, dir, board := lifecycleHome(t)
			name, err := weaveCloneAgentForIssue("lifecycle-parent", 7, dir)
			if err != nil {
				t.Fatal(err)
			}
			var path string
			switch failure {
			case "archive":
				path = filepath.Join(cat.Root(), "agents", "archive")
			case "board":
				path = filepath.Join(board, "queue.json")
			case "room":
				path = filepath.Join(room.Dir(), "members")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("unreadable evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{item(7, "done", name, 0)}}); err != nil {
				t.Fatal(err)
			}
			if _, ok := cat.Agent(name); !ok {
				t.Fatalf("removed with %s failure", failure)
			}
		})
	}
}

func TestEphemeralLifecycleDefersConcurrentClaims(t *testing.T) {
	for _, claim := range []string{"lease", "room"} {
		t.Run(claim, func(t *testing.T) {
			cat, dir, board := lifecycleHome(t)
			name, err := weaveCloneAgentForIssue("lifecycle-parent", 7, dir)
			if err != nil {
				t.Fatal(err)
			}
			q := &weaveQueue{Items: []*weaveItem{item(7, "done", name, 0)}}
			attempt := func() error {
				if err := saveWeaveQueue(dir, q); err != nil {
					return err
				}
				if _, ok := cat.Agent(name); !ok {
					t.Fatal("retired during concurrent claim")
				}
				return nil
			}
			if claim == "room" {
				if err := room.WithMemberClaimsGuard(attempt); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(board, 0700); err != nil {
					t.Fatal(err)
				}
				release, err := weaveFlock(filepath.Join(board, "queue.lock"), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				err = attempt()
				release()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := saveWeaveQueue(dir, q); err != nil {
				t.Fatal(err)
			}
			assertArchived(t, cat, name)
		})
	}
}

func TestEphemeralManagerCloneBindsToRun(t *testing.T) {
	cat, dir, _ := lifecycleHome(t)
	t.Setenv("BASHY_MINT_SPRINT", "sprint-origin")
	a, err := cat.CloneAgent("lifecycle-parent", "managed-clone", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(a); err != nil {
		t.Fatal(err)
	}
	if err := weaveBindEphemeralClone(a.Name, dir, 7); err != nil {
		t.Fatal(err)
	}
	got, _ := cat.Agent(a.Name)
	if got.Lifecycle.RunID != 7 || got.Lifecycle.WeaveQueue != dir || got.Lifecycle.SprintID != "sprint-origin" {
		t.Fatalf("ownership: %+v", got.Lifecycle)
	}
	if err := weaveBindEphemeralClone(a.Name, dir, 8); err == nil {
		t.Fatal("reassigned a run-owned clone")
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{item(7, "done", a.Name, 0)}}); err != nil {
		t.Fatal(err)
	}
	assertArchived(t, cat, a.Name)
}

func TestEphemeralSprintManagerReceivesMintContext(t *testing.T) {
	_, _, board := lifecycleHome(t)
	if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{{ID: 7, UUID: "mint-owner", Owner: "lifecycle-parent"}}}); err != nil {
		t.Fatal(err)
	}
	old := StartSprintOwner
	t.Cleanup(func() { StartSprintOwner = old })
	sentinel := errors.New("launch observed")
	StartSprintOwner = func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		if req.Env["BASHY_MINT_SPRINT"] != "mint-owner" {
			t.Fatalf("missing mint context: %+v", req.Env)
		}
		return SprintOwnerSession{}, sentinel
	}
	_, _, err := ensureSprintOwnerSession(context.Background(), 7, "lifecycle-parent", "manage", t.TempDir(), time.Hour, "test-token")
	if !errors.Is(err, sentinel) {
		t.Fatalf("launch error: %v", err)
	}
}

func TestEphemeralSprintSeatRetiresAfterSessionStops(t *testing.T) {
	cat, _, board := lifecycleHome(t)
	a := fleet.Agent{Name: "ending-seat", Tool: "codex", Model: "gpt5.6-sol", Ephemeral: true, Lifecycle: &fleet.AgentLifecycle{SprintID: "ending-sprint"}}
	if err := cat.SaveAgent(a); err != nil {
		t.Fatal(err)
	}
	if err := room.Join(room.Card{ID: a.Name, Nick: a.Name, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{{ID: 7, UUID: "ending-sprint", Column: "done", Owner: a.Name}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Agent(a.Name); !ok {
		t.Fatal("live ending manager removed")
	}
	old := StopSprintOwner
	t.Cleanup(func() { StopSprintOwner = old })
	StopSprintOwner = func(context.Context, SprintOwnerRequest) error { room.Leave(a.Name); return nil }
	releaseSprintOwnerSession(context.Background(), 7, a.Name, t.TempDir())
	assertArchived(t, cat, a.Name)
}
