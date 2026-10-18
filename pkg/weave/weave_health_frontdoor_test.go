package weave

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// pinHealthTestCatalog pins the fleet registry to the embedded baseline under a
// throwaway root so the alias resolution under test never reads the
// developer's real store.
func pinHealthTestCatalog(t *testing.T) {
	t.Helper()
	root := fleettest.Ring(t)
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })
}

// frontDoorGenieSpec mirrors the live queue record of yoke run #395 (and #389
// before it): the ycode binding launches through the bashy front door, so the
// launch spec's binary is "bashy" and the tool is the first argv token.
func frontDoorGenieSpec(binary string) *weaveLaunchSpec {
	return &weaveLaunchSpec{
		Tool:        binary,
		Argv:        []string{"genie", "-m", "glm-5.3", "Sprint467 bug fixes only."},
		Agent:       "ycode-glm-5.3",
		Model:       "glm-5.3",
		MaxRuntime:  25 * time.Minute,
		IdleTimeout: 10 * time.Minute,
		PTY:         "auto",
	}
}

// A ycode/genie binding launched through the bashy front door is the SAME tool
// as its recorded registry name. Comparing the front-door binary ("bashy")
// against the recorded tool made every such run report "inconsistent" while
// its wrapper was alive and its smoke passed (Sprint #467, story #2371).
func TestHealthAcceptsBashyFrontDoorForRecordedYcodeOrGenie(t *testing.T) {
	pinHealthTestCatalog(t)
	for _, tc := range []struct{ recorded, binary string }{
		{"ycode", "bashy"},
		{"genie", "bashy"},
		{"ycode", "/Users/u/.local/bin/bashy"},
		{"genie", "/Users/u/.local/bin/bashy"},
		{"ycode", `C:\Users\u\.local\bin\bashy.exe`},
	} {
		s := weaveHealthSnapshot{State: "working", Owner: "ycode-glm-5.3-e", Tool: tc.recorded, WrapperPID: 42, WorkspaceExists: true}
		it := &weaveItem{LaunchSpec: frontDoorGenieSpec(tc.binary)}
		if msg, _, bad := weaveHealthConsistencyIssue(s, it); bad {
			t.Errorf("recorded %q launched %q genie: healthy run reported %q", tc.recorded, tc.binary, msg)
		}
	}
}

// The steer surface launches `bashy ycode -m MODEL`: still the recorded tool.
func TestHealthAcceptsBashyFrontDoorYcodeVerb(t *testing.T) {
	pinHealthTestCatalog(t)
	s := weaveHealthSnapshot{State: "working", Owner: "a", Tool: "ycode", WrapperPID: 42, WorkspaceExists: true}
	it := &weaveItem{LaunchSpec: &weaveLaunchSpec{Tool: "bashy", Argv: []string{"ycode", "-m", "glm-5.3"}}}
	if msg, _, bad := weaveHealthConsistencyIssue(s, it); bad {
		t.Fatalf("bashy ycode launch recorded as ycode reported %q", msg)
	}
}

// The end-to-end symptom: a live wrapper on a front-door launch must not be
// classified inconsistent — the real run was moved to another binding on the
// strength of that false reading.
func TestHealthLiveFrontDoorRunIsNotInconsistent(t *testing.T) {
	pinHealthTestCatalog(t)
	it := &weaveItem{
		ID: 395, State: "working", Owner: "ycode-glm-5.3-e", Tool: "ycode",
		Workspace: "/workspace/395", StartedAt: time.Unix(100, 0).UTC(),
		LaunchSpec: frontDoorGenieSpec("bashy"), WrapperPid: 54060,
	}
	now := it.StartedAt.Add(time.Minute)
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, true))
	got := weaveClassifyHealth(s, it, now)
	if got.Health == weaveHealthInconsistent {
		t.Fatalf("live front-door run classified inconsistent: %+v", got)
	}
}

// A front-door launch of a genuinely different tool is still a contradiction,
// and a front door with nothing after it identifies no tool at all.
func TestHealthStillFlagsForeignToolBehindBashyFrontDoor(t *testing.T) {
	pinHealthTestCatalog(t)
	for _, tc := range []struct {
		name, recorded string
		spec           *weaveLaunchSpec
	}{
		{"codex recorded, genie launched", "codex", frontDoorGenieSpec("bashy")},
		{"claude recorded, genie launched", "claude", frontDoorGenieSpec("/Users/u/.local/bin/bashy")},
		{"ycode recorded, codex launched via front door", "ycode", &weaveLaunchSpec{Tool: "bashy", Argv: []string{"codex", "exec", "-p"}}},
		{"ycode recorded, bare front door", "ycode", &weaveLaunchSpec{Tool: "bashy"}},
		{"ycode recorded, front door with a flag", "ycode", &weaveLaunchSpec{Tool: "bashy", Argv: []string{"-c", "genie"}}},
		{"ycode recorded, plain claude binary", "ycode", &weaveLaunchSpec{Tool: "/Users/u/.bashy/tools/claude/2.1.283/claude", Argv: []string{"-p", "x"}}},
	} {
		s := weaveHealthSnapshot{State: "working", Owner: "a", Tool: tc.recorded, WrapperPID: 42, WorkspaceExists: true}
		it := &weaveItem{LaunchSpec: tc.spec}
		msg, _, bad := weaveHealthConsistencyIssue(s, it)
		if !bad || !strings.Contains(msg, "contradicts recorded tool") {
			t.Errorf("%s: must be flagged, got bad=%v %q", tc.name, bad, msg)
		}
	}
}
