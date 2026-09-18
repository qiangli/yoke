package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// pinAgentFleet gives weave a scratch registry over the test ring, holding
// one nicknamed agent of its own.
func pinAgentFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	fleettest.Ring(t)
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{
		Name: "007", Aliases: []string{"smarty"}, Tool: "claude", Model: "fable",
	}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })
	return cat
}

// THE ASK: `weave start -- 007` launches claude with 007's model, and the
// issue body as the prompt.
func TestExpandAgentSelectsTheModel(t *testing.T) {
	pinAgentFleet(t)
	l, argv, err := weaveExpandAgent([]string{"007"}, "FIX THE GATE", "title")
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		t.Fatal("a nickname must expand")
	}
	wantPrefix := "claude --dangerously-skip-permissions --model claude-fable-5 --output-format stream-json --verbose -p FIX THE GATE"
	if got := strings.Join(argv, " "); !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("argv =\n  %q\nwant prefix\n  %q", got, wantPrefix)
	}
	// Every worker prompt carries the standard commit-or-it's-lost contract.
	if !strings.Contains(argv[len(argv)-1], "WEAVE WORKER CONTRACT") {
		t.Fatalf("worker contract not appended to the prompt: %q", argv[len(argv)-1])
	}
	// Bound as `fable`, recorded as `claude:fable5`: the binding is canonical
	// however it was spelled.
	if l.Nick != "007" || l.Binding() != "claude:fable5" {
		t.Fatalf("launch = %+v", l)
	}
}

func TestExpandedAgentEnablesDeclaredStdoutEventsOnce(t *testing.T) {
	pinAgentFleet(t)
	_, argv, err := weaveExpandAgent([]string{"007"}, "body", "title")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if strings.Count(got, "--output-format stream-json --verbose") != 1 {
		t.Fatalf("Claude event flags must appear exactly once: %q", got)
	}
	if !strings.HasPrefix(argv[len(argv)-1], "body") {
		t.Fatalf("event flags displaced the final prompt: %q", argv)
	}
	if twice := weaveAgentEventsStdoutArgv(&weaveAgentLaunch{ToolName: "claude"}, argv); strings.Join(twice, "\x00") != strings.Join(argv, "\x00") {
		t.Fatalf("event flag injection is not idempotent:\n once=%q\n twice=%q", argv, twice)
	}
}

func TestExpandedYcodeEnablesDeclaredEventFile(t *testing.T) {
	pinAgentFleet(t)
	l, argv, err := weaveExpandAgent([]string{"ycode:glm-5.2"}, "body", "title")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	argv = weaveAgentEventsFileArgv(l, argv, path)
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "--events "+path) {
		t.Fatalf("ycode event file flag missing: %q", got)
	}
	if !strings.HasPrefix(argv[len(argv)-1], "body") {
		t.Fatalf("event file flags displaced the final prompt: %q", argv)
	}
	if twice := weaveAgentEventsFileArgv(l, argv, path); strings.Join(twice, "\x00") != strings.Join(argv, "\x00") {
		t.Fatalf("event file flag injection is not idempotent:\n once=%q\n twice=%q", argv, twice)
	}
}

func TestExpandedAGYKeepsPromptFlagAdjacentToPrompt(t *testing.T) {
	pinAgentFleet(t)
	_, argv, err := weaveExpandAgent([]string{"agy:gemini3.1"}, "THE TASK", "title")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "--output-format stream-json -p THE TASK") {
		t.Fatalf("AGY event flags split -p from its prompt: %q", got)
	}
	if argv[len(argv)-2] != "-p" || !strings.HasPrefix(argv[len(argv)-1], "THE TASK") {
		t.Fatalf("AGY prompt pair is not final and adjacent: %q", argv)
	}
}

// An alias names the same agent.
func TestExpandAgentByAlias(t *testing.T) {
	pinAgentFleet(t)
	l, _, err := weaveExpandAgent([]string{"smarty"}, "body", "title")
	if err != nil || l == nil {
		t.Fatalf("l=%+v err=%v", l, err)
	}
	if l.Nick != "007" {
		t.Fatalf("alias must resolve to the canonical nickname, got %q", l.Nick)
	}
}

// A registered nickname never silently changes meaning from "agent" to
// "executable" just because weave flags were placed after `--`. This exact
// typo used to hydrate a full workspace and then fail with exec status 127.
func TestExpandAgentRejectsArgumentsAfterNickname(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{
		Name: "worker", Nick: "Zephyr", Tool: "opencode", Model: "kimi-k2.7-code",
	}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	l, argv, err := weaveExpandAgent([]string{"Zephyr", "--json", "--quiet"}, "body", "title")
	if err == nil || !strings.Contains(err.Error(), "registered agent") || !strings.Contains(err.Error(), "before `--`") {
		t.Fatalf("launch=%+v argv=%q err=%v", l, argv, err)
	}
	if l != nil || argv != nil {
		t.Fatalf("rejected launch leaked executable state: launch=%+v argv=%q", l, argv)
	}
	if l, argv, err := weaveExpandAgent([]string{"sh", "-c", "true"}, "body", "title"); err != nil || l != nil || argv != nil {
		t.Fatalf("raw executable argv changed: launch=%+v argv=%q err=%v", l, argv, err)
	}
}

func TestExpandAGYCarriesGenericWorkspacePlaceholder(t *testing.T) {
	pinAgentFleet(t)
	l, argv, err := weaveExpandAgent([]string{"agy:gemini3.1"}, "body", "title")
	if err != nil || l == nil {
		t.Fatalf("launch=%+v err=%v", l, err)
	}
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "agy --new-project --add-dir {workspace} --dangerously-skip-permissions") {
		t.Fatalf("agy expansion did not carry workspace binding before writer args: %q", got)
	}
	if len(l.WorkspacePreflight) == 0 {
		t.Fatal("agy expansion must carry its named workspace preflight command")
	}
	bound, err := weaveBindAgentWorkspace(l, argv, "/tmp/allocated-work")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(bound, " "); strings.Contains(got, fleet.WorkspaceToken) || !strings.Contains(got, "--new-project --add-dir /tmp/allocated-work") {
		t.Fatalf("bound argv = %q", got)
	}
}

func TestWorkspacePreflightVerificationFailsClosed(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "allocated")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, report := range []string{workspace, "PWD=" + workspace, "PROJECT_DIR=" + workspace} {
		if err := weaveVerifyReportedWorkspace(report, workspace); err != nil {
			t.Errorf("valid report %q: %v", report, err)
		}
	}
	for _, report := range []string{"", "PWD=/somewhere/else", "PWD=" + workspace + "\nextra"} {
		if err := weaveVerifyReportedWorkspace(report, workspace); err == nil {
			t.Errorf("invalid report %q was accepted", report)
		}
	}
}

func TestWorkspacePreflightRunsDeclaredCommand(t *testing.T) {
	workspace := t.TempDir()
	l := &weaveAgentLaunch{WorkspacePreflight: []string{os.Args[0], "-test.run=TestWorkspacePreflightHelper", "--"}}
	if err := weaveRunWorkspacePreflight(l, workspace, append(os.Environ(), "WEAVE_TEST_PREFLIGHT=1")); err != nil {
		t.Fatal(err)
	}
	wrong := append(os.Environ(), "WEAVE_TEST_PREFLIGHT=1", "WEAVE_TEST_PREFLIGHT_REPORT=/wrong/project")
	if err := weaveRunWorkspacePreflight(l, workspace, wrong); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatched reported project must fail closed: %v", err)
	}
}

func TestWorkspacePreflightHelper(t *testing.T) {
	if os.Getenv("WEAVE_TEST_PREFLIGHT") != "1" {
		return
	}
	report := os.Getenv("WEAVE_TEST_PREFLIGHT_REPORT")
	if report == "" {
		var err error
		report, err = os.Getwd()
		if err != nil {
			os.Exit(2)
		}
	}
	fmt.Printf("PWD=%s\n", report)
	os.Exit(0)
}

// A tool:model binding resolves through the registry. The baseline nicknames
// each seeded pair, so the binding lands on that CANONICAL nickname — and the
// worker is stamped with it, because `whois opencode-deepseek-v4` resolves.
func TestBindingResolvesToItsCanonicalNickname(t *testing.T) {
	pinAgentFleet(t)
	l, argv, err := weaveExpandAgent([]string{"opencode:deepseek-v4-pro"}, "body", "title")
	if err != nil || l == nil {
		t.Fatalf("l=%+v err=%v", l, err)
	}
	// The provider-side id is what reaches --model.
	if !strings.Contains(strings.Join(argv, " "), "--model deepseek/deepseek-v4-pro") {
		t.Fatalf("argv = %q", argv)
	}
	if l.Nick != "opencode-deepseek-v4-pro" {
		t.Fatalf("nick = %q, want the canonical nickname of the seeded pair", l.Nick)
	}
	if got := weaveAgentEnv(nil, l); !hasKV(got, "BASHY_PRINCIPAL=dhnt:agent/opencode-deepseek-v4-pro") {
		t.Fatalf("env = %q", got)
	}
}

// A binding NOBODY has nicknamed still launches with the right model, but it
// is not a principal: a mention cannot carry a colon, so `@aider:opus` would
// never resolve.
func TestUnNicknamedBindingIsNotAPrincipal(t *testing.T) {
	pinAgentFleet(t)
	l, argv, err := weaveExpandAgent([]string{"aider:opus"}, "body", "title")
	if err != nil || l == nil {
		t.Fatalf("l=%+v err=%v", l, err)
	}
	if l.Nick != "aider:opus" {
		t.Fatalf("nick = %q", l.Nick)
	}
	if !strings.Contains(strings.Join(argv, " "), "--model claude-opus-5") {
		t.Fatalf("argv = %q", argv)
	}
	base := []string{"PATH=/bin"}
	if got := weaveAgentEnv(base, l); len(got) != len(base) {
		t.Fatalf("an un-nicknamed binding must not be stamped as a principal: %q", got)
	}
}

// The issue body is the prompt, and it stays the final argument — aider takes
// its prompt as the value of --message.
func TestExpandedBodyIsTheFinalArgument(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{Name: "surgeon", Tool: "aider", Model: "deepseek-v4"}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	_, argv, err := weaveExpandAgent([]string{"surgeon"}, "THE BODY", "title")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(argv[len(argv)-1], "THE BODY") {
		t.Fatalf("body is not the leading content of the last arg: %q", argv)
	}
	if argv[len(argv)-2] != "--message" {
		t.Fatalf("aider's prompt must be the value of --message: %q", argv)
	}
}

// --- everything else is left exactly alone -------------------------------

// A bare tool name keeps its current meaning: a raw launch. Rewriting what
// `-- claude` spawns would silently change every conductor script.
func TestBareToolNameIsNotExpanded(t *testing.T) {
	pinAgentFleet(t)
	for _, name := range []string{"claude", "codex", "sh"} {
		l, _, err := weaveExpandAgent([]string{name}, "body", "title")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if l != nil {
			t.Fatalf("%s: a bare tool name must not expand, got %+v", name, l)
		}
	}
}

// A multi-token argv is the conductor speaking deliberately.
func TestMultiTokenArgvIsVerbatim(t *testing.T) {
	pinAgentFleet(t)
	for _, argv := range [][]string{
		{"claude", "--dangerously-skip-permissions", "-p"},
		{"sh", "-c", "echo hi"},
		{"007", "--extra"}, // even an agent name, once the operator adds args
	} {
		l, argv, err := weaveExpandAgent(argv, "body", "title")
		if err != nil || l != nil {
			t.Fatalf("%q: l=%+v err=%v — a written argv is honored as written", argv, l, err)
		}
	}
}

func TestRegisteredAgentRejectsMisplacedWeaveFlags(t *testing.T) {
	pinAgentFleet(t)
	for _, argv := range [][]string{
		{"007", "--json"},
		{"007", "--quiet", "--plain"},
		{"007", "--max-runtime=5m"},
	} {
		l, _, err := weaveExpandAgent(argv, "body", "title")
		if err == nil || l != nil || !strings.Contains(err.Error(), "before `--`") {
			t.Fatalf("%q: launch=%+v err=%v", argv, l, err)
		}
	}
}

// An unknown single token is not an agent, so it is passed through and fails
// (or succeeds) exactly as it does today.
func TestUnknownTokenIsNotExpanded(t *testing.T) {
	pinAgentFleet(t)
	l, _, err := weaveExpandAgent([]string{"my-own-script"}, "body", "title")
	if err != nil || l != nil {
		t.Fatalf("l=%+v err=%v", l, err)
	}
}

// --- loud failures --------------------------------------------------------

// Binding a model to a tool that cannot select one is a label, not a
// selection. Weave must refuse rather than launch the wrong model.
func TestExpandRefusesAToolThatCannotSelectAModel(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "dumb", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "dumb", Launch: fleet.ToolLaunch{Exec: "dumb {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	_, _, err := weaveExpandAgent([]string{"dumb:opus"}, "body", "title")
	if err == nil || !strings.Contains(err.Error(), "cannot select a model") {
		t.Fatalf("err = %v", err)
	}
}

// A detection-only harness has no headless template; a bare launch would hang
// at its trust prompt, so weave says so instead of spawning it.
func TestExpandRefusesAToolWithNoLaunchTemplate(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{Name: "ghosty", Tool: "cursor", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	_, _, err := weaveExpandAgent([]string{"ghosty"}, "body", "title")
	if err == nil {
		t.Fatal("expected a refusal for a harness with no launch template")
	}
}

func TestExpandReportsAMissingTool(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{Name: "orphan", Tool: "nope", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	_, _, err := weaveExpandAgent([]string{"orphan"}, "body", "title")
	if err == nil || !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("err = %v", err)
	}
}

// --- the seat vs the principal ---------------------------------------------

// WEAVE_AGENT is the per-issue SEAT (007-a); BASHY_PRINCIPAL is the AGENT
// (007). `whois 007` resolves; `whois 007-a` does not, so the seat must never
// land in the principal slot.
func TestAgentEnvSeparatesSeatFromPrincipal(t *testing.T) {
	l := &weaveAgentLaunch{Nick: "007", ToolName: "claude", ModelName: "fable"}
	env := weaveAgentEnv([]string{"WEAVE_AGENT=007-a", "PATH=/bin"}, l)

	if !hasKV(env, "WEAVE_AGENT=007-a") {
		t.Error("the seat must survive")
	}
	if !hasKV(env, "BASHY_PRINCIPAL=dhnt:agent/007") ||
		!hasKV(env, "BASHY_AGENT_ID=007") ||
		!hasKV(env, "BASHY_AGENT_BINDING=claude:fable") {
		t.Fatalf("env = %q", env)
	}
}

// A worker must not inherit the conductor's identity.
func TestAgentEnvOverwritesAnInheritedPrincipal(t *testing.T) {
	l := &weaveAgentLaunch{Nick: "007", ToolName: "claude", ModelName: "fable"}
	env := weaveAgentEnv([]string{"BASHY_PRINCIPAL=dhnt:agent/conductor", "BASHY_AGENT_ID=conductor"}, l)
	if hasKV(env, "BASHY_AGENT_ID=conductor") || hasKV(env, "BASHY_PRINCIPAL=dhnt:agent/conductor") {
		t.Fatalf("stale identity survived: %q", env)
	}
	if !hasKV(env, "BASHY_AGENT_ID=007") {
		t.Fatalf("env = %q", env)
	}
}

// A non-agent launch stamps nothing.
func TestAgentEnvNoOpWithoutAnAgent(t *testing.T) {
	base := []string{"PATH=/bin"}
	if got := weaveAgentEnv(base, nil); len(got) != len(base) {
		t.Fatalf("env = %q", got)
	}
}

// The seat name is derived from the agent, not the tool, when one is named.
func TestSeatIsDerivedFromTheAgentNickname(t *testing.T) {
	if got := weaveAgentName("007", 1); got != "007-a" {
		t.Fatalf("seat = %q, want 007-a", got)
	}
	if got := weaveAgentName("claude", 2); got != "claude-b" {
		t.Fatalf("seat = %q, want claude-b", got)
	}
}

func hasKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// The body is the prompt; an issue without one falls back to its title rather
// than handing the tool an empty argument it reads as "no task".
func TestEmptyBodyFallsBackToTheTitle(t *testing.T) {
	pinAgentFleet(t)
	_, argv, err := weaveExpandAgent([]string{"007"}, "   ", "FIX THE GATE")
	if err != nil {
		t.Fatal(err)
	}
	if got := argv[len(argv)-1]; !strings.HasPrefix(got, "FIX THE GATE") {
		t.Fatalf("prompt = %q, want the title as the leading content", got)
	}
}

// No body and no title is not a task. Refuse rather than spawn an agent with
// an empty prompt.
func TestNoPromptIsRefused(t *testing.T) {
	pinAgentFleet(t)
	if _, _, err := weaveExpandAgent([]string{"007"}, "", ""); err == nil {
		t.Fatal("expected a refusal when the issue carries no prompt")
	}
}

// The prompt is never an empty argv element.
func TestPromptIsNeverEmpty(t *testing.T) {
	pinAgentFleet(t)
	_, argv, err := weaveExpandAgent([]string{"007"}, "", "T")
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range argv {
		if a == "" {
			t.Fatalf("argv[%d] is empty: %q", i, argv)
		}
	}
}
