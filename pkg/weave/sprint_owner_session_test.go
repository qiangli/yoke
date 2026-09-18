package weave

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

const sprintOwnerQueueProbeEnv = "BASHY_TEST_SPRINT_OWNER_QUEUE_PROBE"

func withSprintOwnerLauncher(t *testing.T, fn func(context.Context, SprintOwnerRequest) (SprintOwnerSession, error)) {
	t.Helper()
	old := StartSprintOwner
	StartSprintOwner = fn
	t.Cleanup(func() { StartSprintOwner = old })
}

func withSprintOwnerStopper(t *testing.T, fn func(context.Context, SprintOwnerRequest) error) {
	t.Helper()
	old := StopSprintOwner
	StopSprintOwner = fn
	t.Cleanup(func() { StopSprintOwner = old })
}

func publishManagedSprintOwner(t *testing.T, name string) {
	t.Helper()
	id := room.AgentClaimID(name)
	if err := room.Join(room.Card{ID: id, Nick: name, Mode: "foreman", PID: os.Getpid(), Caps: []string{room.CapInboxDelivery}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave(id) })
}

func TestSprintStartInstructionRequiresExplicitOwnerAndDeliversOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "manager")
	if out, code := runSprint(t, "add", "managed sprint"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}

	var got []SprintOwnerRequest
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		got = append(got, req)
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "sprint-1-manager", Transport: room.TransportManaged}, nil
	})

	if out, code := runSprint(t, "start", "1", "--instruction", "do not guess"); code == 0 {
		t.Fatalf("implicit owner accepted: exit=%d %s", code, out)
	}
	if len(got) != 0 {
		t.Fatalf("launcher called for an ambiguous owner: %+v", got)
	}

	exact := "  preserve $HOME; do not parse  "
	out, code := runSprint(t, "start", "1", "--owner", "manager", "--for", "1h", "--instruction", exact)
	if code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	if len(got) != 1 || got[0].Brief != exact || got[0].Owner != "manager" || got[0].Duration != time.Hour {
		t.Fatalf("launcher requests = %+v", got)
	}
	for _, want := range []string{"sprint #1", "manager", "managed", "sprint-1-manager", "meet"} {
		if !strings.Contains(out, want) {
			t.Errorf("start output missing %q: %s", want, out)
		}
	}
}

func TestSprintStartDoesNotChooseManagerFromAmbientIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("WEAVE_CONDUCTOR", "ambient-manager")
	seedLiveAgent(t, "ambient-manager")
	if out, code := runSprint(t, "add", "explicit manager test"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--for", "1h"); code == 0 || !strings.Contains(out, "--owner is required") {
		t.Fatalf("start chose an ambient manager: exit=%d output=%s", code, out)
	}
}

func TestSprintStartAndTakeRejectEmptyExplicitOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	seedAgent(t, "manager")
	if out, code := runSprint(t, "add", "empty owner"); code != 0 {
		t.Fatal(out)
	}
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	for _, verb := range []string{"start", "take"} {
		if out, code := runSprint(t, verb, "1"); code == 0 || !strings.Contains(out, "--owner is required") {
			t.Errorf("%s chose an ambient owner: exit=%d %s", verb, code, out)
		}
		if out, code := runSprint(t, verb, "1", "--owner", ""); code == 0 || !strings.Contains(out, "--owner is required") {
			t.Errorf("%s accepted an empty explicit owner: exit=%d %s", verb, code, out)
		}
	}
}

func TestSprintStartCanonicalizesAliasBeforeLeaseComparison(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	if err := fleet.New().SaveAgent(fleet.Agent{Name: "claude-fable5", Aliases: []string{"Sable"}, Tool: "claude", Model: "fable5"}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "alias owner"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "take", "1", "--owner", "Sable"); code != 0 {
		t.Fatalf("take alias exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Sable", "--for", "1h"); code != 0 {
		t.Fatalf("start alias exit=%d: %s", code, out)
	}
	dir, _ := sprintStoreDir()
	q, _ := readWeaveQueue(dir)
	if got := findWeaveStory(q, 1).Owner; got != "claude-fable5" {
		t.Fatalf("canonical owner = %q", got)
	}
}

func TestSprintStopAndEndStopManagedOwnerSession(t *testing.T) {
	for _, verb := range []string{"stop", "end"} {
		t.Run(verb, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
			seedAgent(t, "manager")
			if out, code := runSprint(t, "add", verb+" lifecycle"); code != 0 {
				t.Fatal(out)
			}
			if out, code := runSprint(t, "start", "1", "--owner", "manager", "--for", "1h"); code != 0 {
				t.Fatal(out)
			}
			var stopped []SprintOwnerRequest
			withSprintOwnerStopper(t, func(_ context.Context, req SprintOwnerRequest) error {
				stopped = append(stopped, req)
				return nil
			})
			args := []string{verb, "1"}
			if out, code := runSprint(t, args...); code != 0 {
				t.Fatalf("%s exit=%d: %s", verb, code, out)
			}
			if len(stopped) != 1 || stopped[0].Sprint != 1 || stopped[0].Owner != "manager" {
				t.Fatalf("stop requests = %+v", stopped)
			}
		})
	}
}

func TestSprintTakeRetiresOldManagerBeforeNewOwnerCanBeInstructed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "old-manager")
	seedAgent(t, "new-manager")
	if out, code := runSprint(t, "add", "manager transfer"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "old-manager", "--for", "1h"); code != 0 {
		t.Fatal(out)
	}
	var retired []SprintOwnerRequest
	withSprintOwnerStopper(t, func(_ context.Context, req SprintOwnerRequest) error {
		retired = append(retired, req)
		return nil
	})
	if out, code := runSprint(t, "edit", "1", "--owner", "new-manager"); code == 0 || !strings.Contains(out, "sprint take") {
		t.Fatalf("active edit changed owner without takeover: exit=%d %s", code, out)
	}
	if len(retired) != 0 {
		t.Fatalf("refused edit stopped the old manager: %+v", retired)
	}
	if out, code := runSprint(t, "take", "1", "--owner", "new-manager", "--force"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	if len(retired) != 1 || retired[0].Owner != "old-manager" {
		t.Fatalf("retired sessions = %+v", retired)
	}
	var launched []SprintOwnerRequest
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		launched = append(launched, req)
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "sprint-1-manager", Transport: room.TransportManaged}, nil
	})
	if out, code := runSprint(t, "instruct", "1", "--instruction", "continue as the new manager"); code != 0 {
		t.Fatalf("instruct exit=%d: %s", code, out)
	}
	if len(launched) != 1 || launched[0].Owner != "new-manager" {
		t.Fatalf("launched sessions = %+v", launched)
	}
}

func TestSprintTakeRefusesOwnerTransferWhenOldSessionCannotStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	seedAgent(t, "old-manager")
	seedAgent(t, "new-manager")
	if out, code := runSprint(t, "add", "blocked transfer"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "old-manager", "--for", "1h"); code != 0 {
		t.Fatal(out)
	}
	withSprintOwnerStopper(t, func(context.Context, SprintOwnerRequest) error { return errors.New("control unavailable") })
	if out, code := runSprint(t, "take", "1", "--owner", "new-manager", "--force"); code == 0 || !strings.Contains(out, "cannot transfer") {
		t.Fatalf("failed teardown allowed takeover: exit=%d %s", code, out)
	}
	dir, _ := sprintStoreDir()
	q, _ := readWeaveQueue(dir)
	if got := findWeaveStory(q, 1).Owner; got != "old-manager" {
		t.Fatalf("failed transfer changed owner to %q", got)
	}
}

func TestSprintInstructionFailureLeavesSprintUnstarted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "manager")
	if out, code := runSprint(t, "add", "failed manager"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	withSprintOwnerLauncher(t, func(context.Context, SprintOwnerRequest) (SprintOwnerSession, error) {
		return SprintOwnerSession{}, errors.New("launch refused")
	})
	if out, code := runSprint(t, "start", "1", "--owner", "manager", "--instruction", "ship it"); code == 0 {
		t.Fatalf("failed delivery reported success: exit=%d %s", code, out)
	}
	dir, _ := sprintStoreDir()
	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s.Column != "backlog" || s.currentBox() != nil || s.Owner != "" {
		t.Fatalf("failed delivery mutated sprint: %+v", s)
	}
}

func TestSprintInstructReusesCurrentOwnerWithoutChangingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "manager")
	publishManagedSprintOwner(t, "manager")
	var briefs []string
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		briefs = append(briefs, req.Brief)
		return SprintOwnerSession{ID: "sprint-1-manager", Reused: len(briefs) > 1, Transport: room.TransportManaged}, nil
	})
	if out, code := runSprint(t, "add", "managed sprint"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "manager", "--instruction", "first"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "instruct", "1", "--instruction", "follow up"); code != 0 || !strings.Contains(out, "reused manager session") {
		t.Fatalf("instruct exit=%d: %s", code, out)
	}
	if len(briefs) != 2 || briefs[0] != "first" || briefs[1] != "follow up" {
		t.Fatalf("delivered briefs = %#v", briefs)
	}
	dir, _ := sprintStoreDir()
	q, _ := readWeaveQueue(dir)
	if got := findWeaveStory(q, 1).Owner; got != "manager" {
		t.Fatalf("later instruction changed owner to %q", got)
	}
}

func TestSprintOwnerStartAndTakeCallbacksRunOutsideGlobalQueueLock(t *testing.T) {
	if dir := os.Getenv(sprintOwnerQueueProbeEnv); dir != "" {
		l, err := lockfile.TryAcquire(filepath.Join(dir, "queue.lock"), lockfile.Holder{Name: "owner-callback-probe"})
		if err != nil {
			t.Fatalf("managed owner callback ran under queue.lock: %v", err)
		}
		_ = l.Release()
		return
	}
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", dir)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "old-manager")
	seedAgent(t, "new-manager")
	probe := func() error {
		child := exec.Command(os.Args[0], "-test.run=^TestSprintOwnerStartAndTakeCallbacksRunOutsideGlobalQueueLock$")
		child.Env = append(os.Environ(), sprintOwnerQueueProbeEnv+"="+dir)
		if out, err := child.CombinedOutput(); err != nil {
			return fmt.Errorf("queue probe: %w: %s", err, out)
		}
		return nil
	}
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		if err := probe(); err != nil {
			return SprintOwnerSession{}, err
		}
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "sprint-1-manager", Transport: room.TransportManaged}, nil
	})
	withSprintOwnerStopper(t, func(context.Context, SprintOwnerRequest) error { return probe() })
	if out, code := runSprint(t, "add", "queue boundary"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "old-manager", "--instruction", "begin"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "take", "1", "--owner", "new-manager", "--force"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "add", "inactive owner edit"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "edit", "2", "--owner", "old-manager"); code != 0 {
		t.Fatalf("initial edit exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "edit", "2", "--owner", "new-manager"); code != 0 {
		t.Fatalf("transfer edit exit=%d: %s", code, out)
	}
}

func TestSprintStartCommitFailureStopsOnlyNewlyLaunchedManager(t *testing.T) {
	for _, reused := range []bool{false, true} {
		t.Run(fmt.Sprintf("reused=%t", reused), func(t *testing.T) {
			home := t.TempDir()
			dir := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("BASHY_SPRINT_DIR", dir)
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			seedAgent(t, "manager")
			if out, code := runSprint(t, "add", "save failure"); code != 0 {
				t.Fatal(out)
			}
			withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
				publishManagedSprintOwner(t, req.Owner)
				queuePath := filepath.Join(dir, "queue.json")
				if err := os.Remove(queuePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(queuePath, 0o700); err != nil {
					t.Fatal(err)
				}
				return SprintOwnerSession{ID: "sprint-1-manager", Reused: reused, Transport: room.TransportManaged}, nil
			})
			stops := 0
			withSprintOwnerStopper(t, func(context.Context, SprintOwnerRequest) error { stops++; return nil })
			if _, code := runSprint(t, "start", "1", "--owner", "manager", "--instruction", "begin"); code == 0 {
				t.Fatal("queue commit failure reported success")
			}
			wantStops := 1
			if reused {
				wantStops = 0
			}
			if stops != wantStops {
				t.Fatalf("cleanup stops=%d, want %d", stops, wantStops)
			}
		})
	}
}

func TestSprintAbortRefusesToParkWhileManagedManagerStillRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	seedAgent(t, "manager")
	if out, code := runSprint(t, "add", "abort manager"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "manager"); code != 0 {
		t.Fatal(out)
	}
	withSprintOwnerStopper(t, func(context.Context, SprintOwnerRequest) error { return errors.New("still running") })
	if out, code := runSprint(t, "abort", "1"); code == 0 || !strings.Contains(out, "still running") {
		t.Fatalf("abort exit=%d: %s", code, out)
	}
	dir, _ := sprintStoreDir()
	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s.Column != "doing" || s.Lease == nil || s.Owner != "manager" {
		t.Fatalf("failed teardown parked sprint: %+v", s)
	}
}
