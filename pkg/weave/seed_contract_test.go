package weave

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// pinFleet points weave's launch contracts at the compiled-in baseline only,
// so a developer's own tool overrides cannot change what these tests see.
func pinFleet(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })
}

// THE GOLDEN TEST. Weave's headless invocations must match the fleet baseline.
// Keep this table explicit so permission and prompt-mode flags cannot drift.
func TestSeededContractArgsUnchanged(t *testing.T) {
	pinFleet(t)
	legacy := map[string][]string{
		"claude":   {"--dangerously-skip-permissions", "-p"},
		"codex":    {"exec", "--skip-git-repo-check", "--sandbox", "workspace-write"},
		"agy":      {"--dangerously-skip-permissions", "--print-timeout", "40m", "-p"},
		"opencode": {"--auto", "run"},
	}
	for tool, want := range legacy {
		got, ok := seededContract(tool)
		if !ok {
			t.Errorf("%s: no seeded contract", tool)
			continue
		}
		if strings.Join(got.HeadlessArgs, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s: HeadlessArgs =\n  %q\nwant (baseline table)\n  %q", tool, got.HeadlessArgs, want)
		}
		if got.Tool != tool {
			t.Errorf("%s: Tool = %q", tool, got.Tool)
		}
	}
}

// The per-tool quirks the conductor depends on survived the move.
func TestSeededContractCarriesTheLaunchQuirks(t *testing.T) {
	pinFleet(t)

	claude, _ := seededContract("claude")
	if !claude.SupportsSay || claude.TrustClear != "say:1" {
		t.Errorf("claude is steerable and needs a trust clear: %+v", claude)
	}

	// codex IS steerable. It was recorded as not-steerable for months because
	// nobody ever tried — and because the control channel sent `text + \r` in ONE
	// write, which codex's TUI (bracketed paste + kitty keyboard protocol) reads
	// as a PASTE: the steer landed in its input box and was never submitted.
	// Measured in pkg/agentpty/steer_live_test.go, through the real control socket.
	codex, _ := seededContract("codex")
	if !codex.SupportsSay {
		t.Error("codex IS steerable — measured in pkg/agentpty/steer_live_test.go")
	}
	if !codex.SupportsGracefulQuit {
		t.Error("codex quits gracefully")
	}

	agy, _ := seededContract("agy")
	if agy.AuthHint == "" {
		t.Error("agy needs an interactive sign-in and must say so")
	}

	opencode, _ := seededContract("opencode")
	if !strings.Contains(opencode.Notes, "committed artifacts") {
		t.Errorf("opencode notes lost: %q", opencode.Notes)
	}
}

// A recognized-but-not-drivable harness has no contract: weave must not try to
// launch a tool the registry never gave a launch template.
func TestDetectionOnlyToolHasNoContract(t *testing.T) {
	pinFleet(t)
	for _, name := range []string{"cursor", "goose", "cline", "gemini"} {
		if _, ok := seededContract(name); ok {
			t.Errorf("%s: got a launch contract for a detection-only harness", name)
		}
	}
}

func TestUnknownToolHasNoContract(t *testing.T) {
	pinFleet(t)
	if _, ok := seededContract("nothing-like-this"); ok {
		t.Fatal("an unknown tool must not produce a contract")
	}
}

// An operator can change what weave launches without a rebuild — that is the
// point of moving the contract out of Go.
func TestOperatorCanOverrideTheLaunchContract(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	tl, ok := cat.Tool("claude")
	if !ok {
		t.Fatal("no baseline claude")
	}
	tl.CLI.Launch.Exec = "claude --my-flag {prompt}"
	if err := cat.SaveTool(tl); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = prev })

	got, ok := seededContract("claude")
	if !ok || strings.Join(got.HeadlessArgs, " ") != "--my-flag" {
		t.Fatalf("override ignored: %+v", got)
	}
}
