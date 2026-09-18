package weave

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// isolateHome points weave's per-tool profile store at a scratch HOME. Without
// it these tests would read — and one would WRITE — the developer's real
// ~/.bashy/weave/tools.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
}

// pinAutopilotFleet gives weave a scratch registry with one drivable tool, one
// model, and one nicknamed agent on top of them.
func pinAutopilotFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	isolateHome(t)
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "shellish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --headless --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "seat", Kind: fleet.ModelKindSubscription, UpstreamID: "seat-1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Aliases: []string{"smarty"}, Tool: "shellish", Model: "seat"}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })
	return cat
}

// --- roster resolution ------------------------------------------------------

// A bare tool resolves to itself, unvalidated — exactly as every roster entry
// did before agents existed.
func TestBareToolMemberIsUnvalidated(t *testing.T) {
	pinAutopilotFleet(t)
	m, err := resolveWeaveMember("some-unknown-binary")
	if err != nil {
		t.Fatalf("a bare tool must not be validated: %v", err)
	}
	if m.Tool != "some-unknown-binary" || m.Bin != "some-unknown-binary" || m.IsAgent() {
		t.Fatalf("member = %+v", m)
	}
	if m.Binding() != "" || m.Model() != "" {
		t.Fatal("a bare tool selects no model")
	}
	if m.Label() != "some-unknown-binary" {
		t.Fatalf("label = %q", m.Label())
	}
}

// An agent carries its binding, and its executable is the tool's binary.
func TestAgentMemberCarriesTheBinding(t *testing.T) {
	pinAutopilotFleet(t)
	for _, name := range []string{"007", "smarty", "shellish:seat"} {
		m, err := resolveWeaveMember(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.Tool != "shellish" || m.Bin != "sh" {
			t.Fatalf("%s: member = %+v", name, m)
		}
		if m.Binding() != "shellish:seat" || m.Model() != "seat-1" {
			t.Fatalf("%s: binding=%q model=%q", name, m.Binding(), m.Model())
		}
	}
	// A nickname and an alias both label as the canonical nickname; a bare
	// binding labels as itself, since it has no nickname.
	m, _ := resolveWeaveMember("smarty")
	if m.Label() != "007" {
		t.Fatalf("label = %q", m.Label())
	}
}

// A binding that cannot run is a configuration error the operator hears at
// startup, not at 3am on failover from a dead member.
func TestRosterFailsFastOnAnUnrunnableBinding(t *testing.T) {
	cat := pinAutopilotFleet(t)

	// A metered model with no vault key: nothing to bill against.
	if err := cat.SaveModel(fleet.Model{Name: "keyless", Kind: fleet.ModelKindAPI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "broke", Tool: "shellish", Model: "keyless"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWeaveRoster([]string{"007", "broke"}); err == nil ||
		!strings.Contains(err.Error(), "api_key_ref") {
		t.Fatalf("err = %v, want a refusal naming the unusable model", err)
	}

	// A dangling tool.
	if err := cat.SaveAgent(fleet.Agent{Name: "orphan", Tool: "nope", Model: "seat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWeaveRoster([]string{"orphan"}); err == nil ||
		!strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("err = %v", err)
	}

	// A healthy roster resolves.
	got, err := resolveWeaveRoster([]string{"007", "shellish", "sh"})
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// --- the launch argv --------------------------------------------------------

// With no persisted profile, the agent's argv comes straight from the registry,
// model included.
func TestMemberArgsFromRegistry(t *testing.T) {
	pinAutopilotFleet(t) // isolated HOME: no tool profiles exist

	m, _ := resolveWeaveMember("007")
	if got := strings.Join(m.headlessArgs(), " "); got != "--headless --model seat-1" {
		t.Fatalf("args = %q", got)
	}

	bare, _ := resolveWeaveMember("shellish")
	if got := strings.Join(bare.headlessArgs(), " "); got != "--headless" {
		t.Fatalf("a bare tool selects no model: %q", got)
	}
}

// A self-healed tool profile wins, and the model is layered onto it by flag.
// Discarding the repair to re-render from the registry would reintroduce the
// very flags `fleet interview --live` found broken.
func TestPersistedProfileWinsAndStillGetsTheModel(t *testing.T) {
	pinAutopilotFleet(t)
	toolsDir, err := weaveToolsDir()
	if err != nil {
		t.Skipf("tools dir unavailable: %v", err)
	}
	if err := saveToolProfile(toolsDir, &ToolProfile{
		Tool: "shellish", HeadlessArgs: []string{"--repaired"},
	}); err != nil {
		t.Fatal(err)
	}

	m, _ := resolveWeaveMember("007")
	got := strings.Join(m.headlessArgs(), " ")
	if got != "--repaired --model seat-1" {
		t.Fatalf("args = %q, want the repaired contract plus the model", got)
	}

	// The bare tool keeps the repaired contract, with no model.
	bare, _ := resolveWeaveMember("shellish")
	if got := strings.Join(bare.headlessArgs(), " "); got != "--repaired" {
		t.Fatalf("args = %q", got)
	}
}

// A model flag already present in the profile is REPLACED, never duplicated.
func TestModelFlagIsReplacedNotDuplicated(t *testing.T) {
	got := withModelFlag([]string{"--headless", "--model", "stale", "-x"}, "--model", "fresh")
	if strings.Join(got, " ") != "--headless --model fresh -x" {
		t.Fatalf("args = %q", got)
	}
	got = withModelFlag([]string{"--headless"}, "--model", "fresh")
	if strings.Join(got, " ") != "--headless --model fresh" {
		t.Fatalf("args = %q", got)
	}
}

// --- the loop ---------------------------------------------------------------

// Autopilot fails over between BINDINGS, and the winning binding is recorded.
// Two nicknames of the SAME binding are the same member: a binding is the
// identity, and `shellish:seat` names the agent already nicknamed 007.
func TestSameBindingResolvesToOneMember(t *testing.T) {
	pinAutopilotFleet(t)
	a, _ := resolveWeaveMember("007")
	b, _ := resolveWeaveMember("shellish:seat")
	if a.Binding() != b.Binding() || a.Label() != b.Label() {
		t.Fatalf("%q and %q are the same agent: %+v vs %+v", "007", "shellish:seat", a, b)
	}
}

// Autopilot fails over between BINDINGS — one tool, two models — and records
// the binding that finally succeeded.
func TestAutopilotFailsOverBetweenBindings(t *testing.T) {
	cat := pinAutopilotFleet(t)
	if err := cat.SaveModel(fleet.Model{Name: "seat2", Kind: fleet.ModelKindSubscription, UpstreamID: "seat-2"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "backup", Tool: "shellish", Model: "seat2"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	roster, err := resolveWeaveRoster([]string{"007", "backup"})
	if err != nil {
		t.Fatal(err)
	}
	if roster[0].Binding() == roster[1].Binding() {
		t.Fatal("the roster must hold two distinct bindings")
	}
	// One tool, two models: failover here is a MODEL change, which a tool-keyed
	// roster could never express.
	if roster[0].Tool != roster[1].Tool {
		t.Fatal("both bindings share one tool")
	}

	runner := &testWeaveAutopilotRunner{
		actions: map[string][]testWeaveAutopilotRun{
			"007":    {{output: "529 overloaded\n"}}, // primary trips the overload signature
			"backup": {{exit: 0}},                    // the other model succeeds
		},
	}
	res, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir: dir,
		repoRoot: dir,
		fleet:    roster,
		runner:   runner,
		maxRuns:  4,
		sleep:    func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != "backup" || res.Binding != "shellish:seat2" {
		t.Fatalf("the winning BINDING must be recorded, got %+v", res)
	}
	if res.Tool != "shellish" {
		t.Fatalf("tool stays the executable's registry name: %+v", res)
	}
	if got := runner.runList(); len(got) != 2 || got[0] != "007" || got[1] != "backup" {
		t.Fatalf("runs = %v; the loop must move to the NEXT binding", got)
	}
}

// The lease records the binding, additively — `tool` keeps meaning what it did.
func TestLeaseRecordsTheBinding(t *testing.T) {
	pinAutopilotFleet(t)
	dir := t.TempDir()
	m, err := resolveWeaveMember("007")
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := acquireWeaveAutopilotLease(dir, "holder-1", m, 4242, time.Minute, time.Now)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, weaveAutopilotLeaseFile))
	if err != nil {
		t.Fatal(err)
	}
	var lease weaveOrchestratorLease
	if err := json.Unmarshal(raw, &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Tool != "shellish" {
		t.Fatalf("tool = %q — it stays the executable's registry name", lease.Tool)
	}
	if lease.Agent != "007" || lease.Binding != "shellish:seat" {
		t.Fatalf("lease = %+v", lease)
	}
}

// A bare-tool lease carries no agent facet, so an existing reader sees exactly
// what it always saw.
func TestBareToolLeaseIsUnchanged(t *testing.T) {
	pinAutopilotFleet(t)
	dir := t.TempDir()
	m, _ := resolveWeaveMember("codex")
	if _, _, err := acquireWeaveAutopilotLease(dir, "h", m, 1, time.Minute, time.Now); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, weaveAutopilotLeaseFile))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["tool"] != "codex" {
		t.Fatalf("tool = %v", got["tool"])
	}
	if _, has := got["agent"]; has {
		t.Fatalf("a bare tool lease must carry no agent facet: %v", got)
	}
	if _, has := got["binding"]; has {
		t.Fatalf("a bare tool lease must carry no binding: %v", got)
	}
}

// The lease log always names the tool, so existing readers keep parsing it, and
// adds the binding when one was named.
func TestMemberLogFields(t *testing.T) {
	pinAutopilotFleet(t)
	agent, _ := resolveWeaveMember("007")
	if got := memberLogFields(agent); got != "tool=shellish agent=007 binding=shellish:seat" {
		t.Fatalf("log = %q", got)
	}
	bare, _ := resolveWeaveMember("codex")
	if got := memberLogFields(bare); got != "tool=codex" {
		t.Fatalf("log = %q", got)
	}
}

// Failback probes the EXECUTABLE. The model was validated when the roster
// resolved, and nothing about a model changes between runs.
func TestHealthyProbesTheBinary(t *testing.T) {
	pinAutopilotFleet(t)
	m, _ := resolveWeaveMember("007")
	if !(weaveExecAutopilotRunner{}).Healthy(context.Background(), m) {
		t.Fatal("the executable is `sh`, which is on PATH")
	}
	missing := weaveMember{Name: "x", Tool: "x", Bin: "definitely-not-on-path-xyz"}
	if (weaveExecAutopilotRunner{}).Healthy(context.Background(), missing) {
		t.Fatal("a missing binary is not healthy")
	}
}
