package weave

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/llmbudget"
)

func TestExactBindingProbeGatesAssignment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	cat := pinFleetWith(t)
	bin := filepath.Join(t.TempDir(), "bound-probe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$2\" in good) echo PROBE_OK;; *) echo model_not_found >&2;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{Name: "bound-probe", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: bin, Launch: fleet.ToolLaunch{Exec: bin + " --model {model} {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"good", "bad"} {
		if err := cat.SaveModel(fleet.Model{Name: name, Kind: fleet.ModelKindSubscription, UpstreamID: name}); err != nil {
			t.Fatal(err)
		}
		if err := cat.SaveAgent(fleet.Agent{Name: name + "-agent", Tool: "bound-probe", Model: name}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	now := time.Now()
	if err := cachedBindingReady(dir, "good-agent", now); err == nil || !strings.Contains(err.Error(), "--probe") {
		t.Fatalf("missing evidence: %v", err)
	}
	cache := map[string]fleetProbeEntry{}
	good, _ := fleetRowForEntry(dir, "good-agent", now, true, cache)
	bad, _ := fleetRowForEntry(dir, "bad-agent", now, true, cache)
	if !good.Capable || bad.Capable || !strings.Contains(bad.ProbeReason, "model") {
		t.Fatalf("good=%+v bad=%+v", good, bad)
	}
	saveFleetProbeCache(dir, cache)
	if err := cachedBindingReady(dir, "good-agent", now); err != nil {
		t.Fatalf("usable binding refused: %v", err)
	}
	if err := cachedBindingReady(dir, "bad-agent", now); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("dead binding accepted: %v", err)
	}
	if err := cachedBindingReady(dir, "good-agent", now.Add(fleetProbeTTL+time.Second)); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("stale evidence accepted: %v", err)
	}
	if err := os.Chtimes(bin, now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := cachedBindingReady(dir, "good-agent", now); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("changed executable reused old probe: %v", err)
	}
	oldAdmission := fleetProbeAdmission
	fleetProbeAdmission = func(_ context.Context, req llmbudget.Request) (llmbudget.Admission, error) {
		if req.Host == "" || req.Model != "good" || req.Agent != "good-agent" || req.HostSlots != 1 {
			t.Fatalf("budget preview lost venue or binding: %+v", req)
		}
		return llmbudget.Admission{Decision: llmbudget.Decision{Action: llmbudget.Block, Reason: "metered spend denied"}}, nil
	}
	t.Cleanup(func() { fleetProbeAdmission = oldAdmission })
	launch, err := weaveResolveAgent("good-agent")
	if err != nil {
		t.Fatal(err)
	}
	cache["agent:"+launch.Binding()] = fleetProbeEntry{Capable: true, Path: bin, Binding: fleetBindingFingerprint(launch, bin), ProbedAt: now}
	saveFleetProbeCache(dir, cache)
	if err := cachedBindingReady(dir, "good-agent", now); err == nil || !strings.Contains(err.Error(), "metered spend denied") {
		t.Fatalf("policy denial accepted at assignment: %v", err)
	}
	blocked, dirty := fleetRowForEntry(dir, "good-agent", now, true, map[string]fleetProbeEntry{})
	if dirty || blocked.Probed || !strings.Contains(blocked.Reason, "metered spend denied") {
		t.Fatalf("policy denial ran probe: %+v dirty=%v", blocked, dirty)
	}
}

// pinFleetWith installs a scratch registry over the test ring and returns its
// catalog for setup.
func pinFleetWith(t *testing.T) *fleet.Catalog {
	t.Helper()
	fleettest.Ring(t)
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })
	return cat
}

func rowFor(t *testing.T, name string) fleetRow {
	t.Helper()
	row, _ := fleetRowForEntry(t.TempDir(), name, time.Now(), false, map[string]fleetProbeEntry{})
	return row
}

// --- the roster ------------------------------------------------------------

// The default roster is unchanged: the tool names it always was.
func TestDefaultRosterIsStillTools(t *testing.T) {
	pinFleetWith(t)
	got := weaveFleetRoster("", false)
	if strings.Join(got, ",") != strings.Join(weaveDefaultFleet, ",") {
		t.Fatalf("default roster = %v, want %v", got, weaveDefaultFleet)
	}
}

// --fleet may name agents, bindings, or tools, mixed.
func TestRosterAcceptsAgentsAndTools(t *testing.T) {
	pinFleetWith(t)
	got := weaveFleetRoster("007, claude:opus ,codex", false)
	if strings.Join(got, "|") != "007|claude:opus|codex" {
		t.Fatalf("roster = %v", got)
	}
}

// --agents expands to every agent in the registry.
func TestAgentsFlagExpandsTheRegistry(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveAgent(fleet.Agent{Name: "zulu", Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	got := weaveFleetRoster("", true)
	var found bool
	for _, n := range got {
		if n == "zulu" {
			found = true
		}
	}
	if !found {
		t.Fatalf("--agents roster is missing the registry's agent: %v", got)
	}
	// The eight seeded bindings plus ours.
	if len(got) < 9 {
		t.Fatalf("--agents roster = %v", got)
	}
}

// --- tool rows are byte-for-byte what they were -----------------------------

// A bare tool name yields exactly the row it always did: no agent facet, no
// model, no reason. Every existing consumer of `weave fleet --json` keeps
// reading what it read before.
func TestBareToolRowIsUnchanged(t *testing.T) {
	pinFleetWith(t)
	row := rowFor(t, "sh") // on PATH everywhere this test runs
	if row.Tool != "sh" {
		t.Fatalf("tool = %q", row.Tool)
	}
	if row.Agent != "" || row.Model != "" || row.Binding != "" || row.Reason != "" {
		t.Fatalf("a bare tool row must carry no agent facet: %+v", row)
	}
	if !row.Found || row.Available {
		t.Fatalf("sh should be installed but unprobed: %+v", row)
	}
}

func TestMissingBareToolIsNotFound(t *testing.T) {
	pinFleetWith(t)
	row := rowFor(t, "definitely-not-on-path-xyz")
	if row.Found || row.Available {
		t.Fatalf("row = %+v", row)
	}
	// Not an error — just absent, exactly as before.
	if row.Reason != "" {
		t.Fatalf("a missing tool is NOT FOUND, not a dangling binding: %q", row.Reason)
	}
}

// --- agent rows -------------------------------------------------------------

// An agent row carries its binding, and its availability still rests on the
// tool's PATH lookup.
func TestAgentRowCarriesTheBinding(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "shellish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "seat", Kind: fleet.ModelKindSubscription, UpstreamID: "seat-1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Aliases: []string{"smarty"}, Tool: "shellish", Model: "seat"}); err != nil {
		t.Fatal(err)
	}

	row := rowFor(t, "007")
	if row.Tool != "shellish" || row.Agent != "007" || row.Binding != "shellish:seat" {
		t.Fatalf("row = %+v", row)
	}
	if row.Model != "seat-1" {
		t.Fatalf("model must be the provider-side id: %q", row.Model)
	}
	if !row.Found || row.Available {
		t.Fatalf("its tool resolves on PATH: %+v", row)
	}

	// An alias names the same agent, and reports the canonical nickname.
	if got := rowFor(t, "smarty"); got.Agent != "007" {
		t.Fatalf("alias row agent = %q", got.Agent)
	}
}

// A tool whose binary differs from its name must not read as NOT FOUND.
func TestAgentRowLooksUpTheBinaryNotTheName(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "renamed", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "seat", Kind: fleet.ModelKindSubscription}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "rn", Tool: "renamed", Model: "seat"}); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, "rn")
	if !row.Found {
		t.Fatalf("the executable is `sh`, not `renamed`: %+v", row)
	}
	if !strings.HasSuffix(row.Path, "sh") {
		t.Fatalf("path = %q", row.Path)
	}
}

// THE AGENT FACET: a metered model with no vault key is not assignable, no
// matter how healthy its tool is. A tool-only board could never say this.
func TestAgentUnavailableWhenItsModelIsUnusable(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "shellish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	// kind=api with no api_key_ref: nothing to bill against.
	if err := cat.SaveModel(fleet.Model{Name: "keyless", Kind: fleet.ModelKindAPI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "broke", Tool: "shellish", Model: "keyless"}); err != nil {
		t.Fatal(err)
	}

	row := rowFor(t, "broke")
	if !row.Found {
		t.Fatal("the tool itself is fine")
	}
	if row.Available {
		t.Fatal("an agent whose model is unusable must not be assignable")
	}
	if !strings.Contains(row.Reason, "keyless") || !strings.Contains(row.Reason, "api_key_ref") {
		t.Fatalf("reason must name the model and the fix: %q", row.Reason)
	}
}

// A dangling binding is reported, never hidden. An orchestrator that never
// sees the row cannot learn why it may not assign the agent.
func TestDanglingBindingIsReportedNotDropped(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveAgent(fleet.Agent{Name: "orphan", Tool: "nope", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, "orphan")
	if row.Available {
		t.Fatal("a dangling binding is not assignable")
	}
	if !strings.Contains(row.Reason, "not in the catalog") {
		t.Fatalf("reason = %q", row.Reason)
	}
	if row.Agent != "orphan" {
		t.Fatalf("the row must still name the agent that failed: %+v", row)
	}
}

// A binding whose tool cannot select a model is a label, not a selection.
func TestAgentOnAModellessToolIsReported(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "dumb", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "mislabelled", Tool: "dumb", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, "mislabelled")
	if row.Available || !strings.Contains(row.Reason, "cannot select a model") {
		t.Fatalf("row = %+v", row)
	}
}

// --- the shared tool half ---------------------------------------------------

// Two agents on one tool share its cooldown: a throttle belongs to the binary
// and its provider, not to the binding.
func TestAgentsOnOneToolShareItsCooldown(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "shellish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"m1", "m2"} {
		if err := cat.SaveModel(fleet.Model{Name: m, Kind: fleet.ModelKindSubscription}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "a1", Tool: "shellish", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "a2", Tool: "shellish", Model: "m2"}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := recordToolCooldown(dir, "shellish", time.Now().Add(time.Hour)); err != nil {
		t.Skipf("cooldown API unavailable: %v", err)
	}
	now := time.Now()
	for _, nick := range []string{"a1", "a2"} {
		row, _ := fleetRowForEntry(dir, nick, now, false, map[string]fleetProbeEntry{})
		if row.Available {
			t.Fatalf("%s: a cooling tool cools every agent on it: %+v", nick, row)
		}
		if row.CoolingUnit == "" {
			t.Fatalf("%s: cooldown not surfaced: %+v", nick, row)
		}
	}
}

// The auth probe launches the agent's REAL argv — with its model flag. A probe
// that omitted it would not exercise the launch about to be made.
func TestAuthProbeUsesTheAgentsRealArgv(t *testing.T) {
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "shellish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "sh", Launch: fleet.ToolLaunch{Exec: "sh --model {model} {prompt}", AuthHint: "sign in"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "seat", Kind: fleet.ModelKindSubscription, UpstreamID: "seat-1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "shellish", Model: "seat"}); err != nil {
		t.Fatal(err)
	}

	row := rowFor(t, "007")
	args, hint := row.headlessArgs()
	if strings.Join(args, " ") != "--model seat-1" {
		t.Fatalf("auth probe argv = %q, want the model flag", args)
	}
	if hint != "sign in" {
		t.Fatalf("auth hint = %q", hint)
	}

	// A bare tool row falls back to its seeded contract.
	bare := rowFor(t, "claude")
	bargs, _ := bare.headlessArgs()
	if strings.Join(bargs, " ") != "--dangerously-skip-permissions -p" {
		t.Fatalf("bare tool probe argv = %q", bargs)
	}
}
