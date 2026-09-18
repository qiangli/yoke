package fleet

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// baseline builds a catalog over the compiled-in tools plus the test ring's
// models and agents, with no local store, so a developer's real
// ~/.config/bashy store cannot influence a test.
func baseline(t *testing.T) *Catalog {
	t.Helper()
	fleettest.Ring(t)
	return New(WithoutLocalStore(), WithRoot(t.TempDir()))
}

func TestBaselineParses(t *testing.T) {
	c := baseline(t)

	tools, errs := c.Tools(false)
	if len(errs) != 0 {
		t.Fatalf("tool parse errors: %v", errs)
	}
	want := map[string]bool{"claude": true, "codex": true, "opencode": true, "aider": true, "agy": true}
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Name] = true
		if !tl.IsCLI() {
			t.Errorf("tool %q is not kind:cli but was listed", tl.Name)
		}
	}
	for n := range want {
		if !got[n] {
			t.Errorf("baseline is missing tool %q", n)
		}
	}

	models, errs := c.Models()
	if len(errs) != 0 {
		t.Fatalf("model parse errors: %v", errs)
	}
	if len(models) < 15 {
		t.Errorf("baseline has %d models, want the full L1-L4 ladder", len(models))
	}

	agents, errs := c.Agents()
	if len(errs) != 0 {
		t.Fatalf("agent parse errors: %v", errs)
	}
	// The ladder grows as providers ship models; the floor is what matters — every
	// provider must reach at least L1..L3, so an L3 role always has a candidate.
	if len(agents) < 15 {
		t.Errorf("baseline has %d agents, want at least the 15 ladder rungs", len(agents))
	}
}

// Every baseline agent must be launchable: both halves of its binding
// resolve. A dangling half is the failure this catalog exists to prevent.
func TestBaselineBindingsResolve(t *testing.T) {
	c := baseline(t)
	agents, _ := c.Agents()
	for _, a := range agents {
		_, tool, model, err := c.Binding(a.Name)
		if err != nil {
			t.Errorf("agent %q: %v", a.Name, err)
			continue
		}
		if !tool.TakesModel() {
			t.Errorf("agent %q binds tool %q, which cannot select a model — the binding is a label, not a selection", a.Name, tool.Name)
		}
		if model.Target() == "" {
			t.Errorf("agent %q: model %q has no target id", a.Name, model.Name)
		}
	}
}

// The capability matrix is keyed by tool:model, never by nickname.
func TestMatrixKeyIsTheBinding(t *testing.T) {
	a := Agent{Name: "007", Aliases: []string{"smarty"}, Tool: "claude", Model: "fable"}
	b := Agent{Name: "bond", Tool: "claude", Model: "fable"}
	if a.MatrixKey() != "claude:fable" || a.MatrixKey() != b.MatrixKey() {
		t.Fatalf("MatrixKey must collapse nicknames: %q vs %q", a.MatrixKey(), b.MatrixKey())
	}
}

// A tool:model binding names its agent even before anyone nicknames it —
// and the model half resolves by any name it answers to, so the floating
// family alias works in a binding just as it does on its own.
func TestAgentResolvesByBinding(t *testing.T) {
	c := baseline(t)
	for q, want := range map[string]string{
		"claude:opus4.8": "opus4.8", // exact IDs remain stable
		"claude:opus":    "opus5",   // family alias follows the newest release
	} {
		a, ok := c.Agent(q)
		if !ok || a.Tool != "claude" || a.Model != want {
			t.Fatalf("Agent(%s) = %+v, %v; want claude:%s", q, a, ok, want)
		}
	}
}

// --- the argv contract -------------------------------------------------

// With no model bound, {model} and its orphaned flag vanish, so a template
// carrying a model flag renders exactly like the flagless argv the
// launcher used before models were selectable.
func TestArgvDropsOrphanedModelFlag(t *testing.T) {
	c := baseline(t)
	legacy := map[string][]string{
		"claude":   {"claude", "--dangerously-skip-permissions", "-p"},
		"codex":    {"codex", "exec", "--skip-git-repo-check", "--sandbox", "workspace-write"},
		"agy":      {"agy", "--dangerously-skip-permissions", "--print-timeout", "40m", "-p"},
		"opencode": {"opencode", "--auto", "run"},
		"aider":    {"aider", "--yes-always", "--no-git", "--message"},
	}
	for name, want := range legacy {
		tool, ok := c.Tool(name)
		if !ok {
			t.Fatalf("no tool %q", name)
		}
		got := tool.Argv("", "THE PROMPT")
		want = append(append([]string{}, want...), "THE PROMPT")
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s Argv(no model) =\n  %q\nwant\n  %q", name, got, want)
		}
	}
}

func TestWorkspaceTokenRendersBeforeModelAndPrompt(t *testing.T) {
	tool := Tool{CLI: ToolCLI{Launch: ToolLaunch{
		Exec:                   "runner --model {model} -p {prompt}",
		WorkspaceArg:           "--add-dir {workspace}",
		WorkspacePreflightExec: "runner --mode plan --model {model} -p {prompt}",
	}}}
	got := tool.ArgvWithWorkspace("/tmp/allocated work", "m", "task")
	want := "runner\x00--add-dir\x00/tmp/allocated work\x00--model\x00m\x00-p\x00task"
	if strings.Join(got, "\x00") != want {
		t.Fatalf("workspace argv = %q", got)
	}
	preflight, ok := tool.WorkspacePreflightArgv("/tmp/allocated work", "m", "report")
	if !ok || strings.Join(preflight, "\x00") != "runner\x00--add-dir\x00/tmp/allocated work\x00--mode\x00plan\x00--model\x00m\x00-p\x00report" {
		t.Fatalf("workspace preflight = %q, %v", preflight, ok)
	}
}

func TestWorkspaceMetadataIsOptIn(t *testing.T) {
	tool := Tool{CLI: ToolCLI{Launch: ToolLaunch{Exec: "runner --model {model} {prompt}"}}}
	got := tool.ArgvWithWorkspace("/tmp/work", "m", "task")
	if strings.Join(got, " ") != "runner --model m task" {
		t.Fatalf("tool without workspace metadata changed: %q", got)
	}
	if _, ok := tool.WorkspacePreflightArgv("/tmp/work", "m", "report"); ok {
		t.Fatal("tool without workspace preflight declared one")
	}
}

func TestBaselineAGYDeclaresWorkspaceBinding(t *testing.T) {
	agy, ok := baseline(t).Tool("agy")
	if !ok {
		t.Fatal("baseline agy missing")
	}
	argv := agy.ArgvWithWorkspace("/tmp/weave-work", "Gemini", "task")
	got := strings.Join(argv, " ")
	want := "agy --new-project --add-dir /tmp/weave-work --dangerously-skip-permissions --print-timeout 40m --model Gemini -p task"
	if got != want {
		t.Fatalf("agy workspace argv = %q, want %q", got, want)
	}
	if strings.Contains(got, "--project") {
		t.Fatalf("agy launch must not reuse a remembered project: %q", got)
	}
	if _, ok := agy.WorkspacePreflightArgv("/tmp/weave-work", "Gemini", "report"); !ok {
		t.Fatal("agy must declare a fail-closed workspace preflight")
	}
}

func TestBaselineYcodeDeclaresProbeAndWorkspaceContracts(t *testing.T) {
	ycode, ok := baseline(t).Tool("ycode")
	if !ok {
		t.Fatal("baseline ycode missing")
	}
	if got := strings.Join(ycode.VersionProbeArgv(), " "); got != "ycode version" {
		t.Fatalf("ycode version probe = %q", got)
	}
	argv := strings.Join(ycode.ArgvWithWorkspace("/tmp/weave-work", "deepseek-v4-pro", "task"), "\x00")
	if !strings.Contains(argv, "--session-dir\x00/tmp/weave-work/.git/ycode-sessions") {
		t.Fatalf("workspace session binding missing: %q", argv)
	}
	if direct := strings.Join(ycode.Argv("deepseek-v4-pro", "task"), " "); direct != "ycode --danger-skip-permissions prompt --model deepseek-v4-pro --print task" {
		t.Fatalf("direct ycode argv changed: %q", direct)
	}
}

func TestArgvSubstitutesModel(t *testing.T) {
	c := baseline(t)
	tool, _ := c.Tool("claude")
	got := tool.Argv("opus", "hi")
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "opus", "-p", "hi"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Argv = %q, want %q", got, want)
	}

	// opencode wants provider/model — the upstream id, not the alias.
	oc, _ := c.Tool("opencode")
	m, _ := c.Model("deepseek-v4-pro")
	got = oc.Argv(m.Target(), "hi")
	want = []string{"opencode", "--auto", "run", "--model", "deepseek/deepseek-v4-pro", "hi"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
}

// A tool with no {model} placeholder cannot select a model: binding it is
// a label, not a selection, and callers must be able to see that.
func TestTakesModel(t *testing.T) {
	yes := Tool{CLI: ToolCLI{Launch: ToolLaunch{Exec: "x --model {model} {prompt}"}}}
	no := Tool{CLI: ToolCLI{Launch: ToolLaunch{Exec: "x {prompt}"}}}
	if !yes.TakesModel() || no.TakesModel() {
		t.Fatal("TakesModel misreports the template")
	}
	if got := no.Argv("opus", "p"); strings.Join(got, " ") != "x p" {
		t.Fatalf("a model-less template must ignore the model: %q", got)
	}
}

// --- dual-accept parsing ------------------------------------------------

// The legacy kit:/type: spelling and the canonical name:/kind: spelling
// must parse to the same tool. Day-1 of the migration accepts both.
func TestParseToolDualAccept(t *testing.T) {
	legacy := []byte("kit: codex\ntype: cli\ncli:\n  binary: codex\n")
	canonical := []byte("name: codex\nkind: cli\ncli:\n  binary: codex\n")

	a, err := ParseTool("codex", legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseTool("codex", canonical, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "codex" || a.Kind != ToolKindCLI {
		t.Fatalf("legacy kit:/type: did not fold into name/kind: %+v", a)
	}
	if a.Name != b.Name || a.Kind != b.Kind {
		t.Fatalf("spellings disagree: %+v vs %+v", a, b)
	}
}

// The canonical spelling wins when a document carries both.
func TestParseToolCanonicalWins(t *testing.T) {
	both := []byte("name: real\nkit: legacy\nkind: cli\ntype: func\n")
	got, err := ParseTool("fallback", both, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "real" || got.Kind != ToolKindCLI {
		t.Fatalf("got %+v, want name=real kind=cli", got)
	}
}

// Emitting is always canonical: a legacy document rewrites to name:/kind:
// the first time it is saved, and never re-emits kit:/type:.
func TestMarshalEmitsCanonicalSpelling(t *testing.T) {
	tl, err := ParseTool("codex", []byte("kit: codex\ntype: cli\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "name: codex") || !strings.Contains(s, "kind: cli") {
		t.Fatalf("canonical keys missing:\n%s", s)
	}
	if strings.Contains(s, "kit:") || strings.Contains(s, "type:") {
		t.Fatalf("legacy keys re-emitted:\n%s", s)
	}
}

// A local entry's bytes ARE the asset Content blob: parse → emit → parse
// is a fixed point, so a definition round-trips to a catalog and back.
func TestMarshalRoundTrips(t *testing.T) {
	c := baseline(t)
	tl, _ := c.Tool("opencode")
	tl.Ring = assetring.RingEmbedded

	out, err := Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseTool("opencode", out, nil)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("emit is not a fixed point:\n--- first ---\n%s\n--- second ---\n%s", out, out2)
	}
	if again.CLI.Launch.Exec != tl.CLI.Launch.Exec {
		t.Fatalf("launch template lost in round trip: %q", again.CLI.Launch.Exec)
	}
	if again.CLI.Launch.ACPExec != tl.CLI.Launch.ACPExec {
		t.Fatalf("ACP launch template lost in round trip: %q", again.CLI.Launch.ACPExec)
	}
}

// Function kits share the tool namespace with agentic CLIs. They are not
// fleet tools and must not appear in a default listing.
func TestFunctionKitsAreNotFleetTools(t *testing.T) {
	overlay := assetring.FileFS(fstest.MapFS{
		"ai.yaml": {Data: []byte("kit: ai\ntype: func\n")},
	}, assetring.RingShared, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirTools, overlay))

	tools, _ := c.Tools(false)
	for _, tl := range tools {
		if tl.Name == "ai" {
			t.Fatal("a type:func kit leaked into the default tool listing")
		}
	}
	all, _ := c.Tools(true)
	var found bool
	for _, tl := range all {
		if tl.Name == "ai" {
			found = true
			if tl.Kind != ToolKindFunc {
				t.Errorf("ai kind = %q, want func", tl.Kind)
			}
		}
	}
	if !found {
		t.Fatal("--all must still show non-cli kits")
	}
}

// --- rings and aliases ---------------------------------------------------

// A local entry shadows the compiled-in baseline. The operator wins.
func TestLocalRingShadowsBaseline(t *testing.T) {
	overlay := assetring.FileFS(fstest.MapFS{
		"claude.yaml": {Data: []byte("name: claude\nkind: cli\ncli:\n  binary: my-claude\n  launch:\n    exec: my-claude {prompt}\n")},
	}, assetring.RingLocal, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirTools, overlay))
	tl, ok := c.Tool("claude")
	if !ok || tl.CLI.Binary != "my-claude" {
		t.Fatalf("local override lost: %+v", tl)
	}
}

// Many nicknames, one binding.
func TestAliasesResolveToOneAgent(t *testing.T) {
	fleettest.Ring(t) // `fable` is the family alias of the ring's fable5
	overlay := assetring.FileFS(fstest.MapFS{
		"bond.yaml": {Data: []byte("agents:\n  - name: \"007\"\n    aliases: [smarty, bond]\n    tool: claude\n    model: fable\n")},
	}, assetring.RingLocal, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirAgents, overlay))
	for _, nick := range []string{"007", "smarty", "bond"} {
		a, ok := c.Agent(nick)
		if !ok {
			t.Fatalf("alias %q did not resolve", nick)
		}
		if a.MatrixKey() != "claude:fable5" {
			t.Fatalf("alias %q resolved to %q", nick, a.MatrixKey())
		}
	}
	if cols := c.CheckAliases(); len(cols) != 0 {
		t.Fatalf("distinct aliases of one agent are not a collision: %v", cols)
	}
}

// One name may never mean two things, or whois would have to guess.
func TestAliasCollisionIsReported(t *testing.T) {
	overlay := assetring.FileFS(fstest.MapFS{
		"a.yaml": {Data: []byte("agents:\n  - name: alpha\n    aliases: [ace]\n    tool: claude\n    model: opus\n")},
		"b.yaml": {Data: []byte("agents:\n  - name: beta\n    aliases: [ace]\n    tool: codex\n    model: gpt-5.5\n")},
	}, assetring.RingLocal, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirAgents, overlay))
	cols := c.CheckAliases()
	if len(cols) != 1 || cols[0].Name != "ace" {
		t.Fatalf("CheckAliases = %v, want one collision on \"ace\"", cols)
	}
	if len(cols[0].Holds) != 2 {
		t.Fatalf("collision must name both holders: %v", cols[0].Holds)
	}
	if got, ok := c.Agent("ace"); ok {
		t.Fatalf("ambiguous alias resolved to %+v; resolution must fail closed", got)
	}
}

func TestAgentsReturnOneRowPerCanonicalName(t *testing.T) {
	overlay := assetring.FileFS(fstest.MapFS{
		"first.yaml":  {Data: []byte("name: duplicate\ntool: claude\nmodel: opus5\n")},
		"second.yaml": {Data: []byte("name: DUPLICATE\ntool: codex\nmodel: gpt-5.5\n")},
	}, assetring.RingLocal, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirAgents, overlay))
	agents, errs := c.Agents()
	count := 0
	for _, a := range agents {
		if strings.EqualFold(a.Name, "duplicate") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("agents list contains %d rows for one NAME, want 1: %+v", count, agents)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate agent NAME") {
		t.Fatalf("duplicate NAME errors = %v", errs)
	}
	for _, query := range []string{"duplicate", "DUPLICATE", "DuPlIcAtE"} {
		if got, ok := c.Agent(query); ok {
			t.Errorf("ambiguous canonical NAME %q resolved to %+v; want fail closed", query, got)
		}
	}
}

// A dangling half is reported by name, never silently dropped.
func TestBindingReportsDanglingHalf(t *testing.T) {
	overlay := assetring.FileFS(fstest.MapFS{
		"ghost.yaml": {Data: []byte("agents:\n  - name: ghost\n    tool: claude\n    model: no-such-model\n")},
	}, assetring.RingLocal, ".yaml")

	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithSource(dirAgents, overlay))
	_, _, _, err := c.Binding("ghost")
	if err == nil || !strings.Contains(err.Error(), "no-such-model") {
		t.Fatalf("err = %v, want it to name the missing model", err)
	}
}

// A person's account name is per-host. Assuming the local $USER exists on
// a remote box is the most common way a cross-host reach fails, so an
// unbound host must report that it is a guess.
func TestPersonOSUserIsPerHost(t *testing.T) {
	p := Person{
		Handle:  "alice",
		OSUsers: map[string]string{"host-a": "alice", "host-b": "al"},
	}
	if u, known := p.OSUserFor("host-b"); !known || u != "al" {
		t.Fatalf("OSUserFor(host-b) = %q, %v", u, known)
	}
	if _, known := p.OSUserFor("host-z"); known {
		t.Fatal("an unbound host must not report a known user")
	}

	p.DefaultOSUser = "fallback"
	if u, known := p.OSUserFor("host-z"); !known || u != "fallback" {
		t.Fatalf("DefaultOSUser not used: %q, %v", u, known)
	}
}
