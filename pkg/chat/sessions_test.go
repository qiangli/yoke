package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	whodb "github.com/qiangli/coreutils/pkg/who"
	"github.com/qiangli/yoke/pkg/room"
)

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// TestAgentIDIsTheIdentityNotTheProcess is the whole model in one assertion:
// the id an agent is known by does not change between runs. It used to carry
// os.Getpid(), which made two launches of one agent look like two agents.
func TestAgentIDIsTheIdentityNotTheProcess(t *testing.T) {
	l := Launch{Nick: "elif", ToolName: "ycode", ModelName: "glm-5.2"}
	if got := agentID(l); got != "elif" {
		t.Fatalf("agentID = %q, want the agent's name %q", got, "elif")
	}
	if a, b := agentID(l), agentID(l); a != b {
		t.Fatalf("agentID must be stable, got %q then %q", a, b)
	}
}

// TestAgentIDForUnnamedLaunchIsItsBinding — an unnamed `tool:model` launch has
// no identity of its own, so it becomes its binding. Still one per host: it
// still resolves to one store and one API key. Naming it is how you get a
// second, which is what `agent clone` writes.
func TestAgentIDForUnnamedLaunchIsItsBinding(t *testing.T) {
	bare := Launch{ToolName: "ycode", ModelName: "glm-5.2"}
	got := agentID(bare)
	if got == "" || strings.ContainsAny(got, ":.") {
		t.Fatalf("agentID = %q, want a sanitized, path-safe label", got)
	}
	// Two models of one tool must NOT collapse onto the same identity — that
	// would silently hand them one conversation store.
	other := agentID(Launch{ToolName: "ycode", ModelName: "deepseek-v4-pro"})
	if got == other {
		t.Fatalf("two bindings of one tool collapsed onto %q", got)
	}
}

// TestSessionPathsAreKeyedByIdentity — every path an agent owns is derived from
// its id, so a second run of the same agent finds its OWN socket, events and
// capture rather than minting new ones and orphaning the old.
func TestSessionPathsAreKeyedByIdentity(t *testing.T) {
	isolateHome(t)

	sockA, err := sessionSock("elif")
	if err != nil {
		t.Fatal(err)
	}
	sockB, _ := sessionSock("elif")
	if sockA != sockB {
		t.Errorf("socket not stable across calls: %q vs %q", sockA, sockB)
	}
	if other, _ := sessionSock("elif2"); other == sockA {
		t.Errorf("two agents share a control socket: %q", sockA)
	}

	evA, err := sessionEventsPath("elif")
	if err != nil {
		t.Fatal(err)
	}
	if other, _ := sessionEventsPath("elif2"); other == evA {
		t.Errorf("two agents share an events stream: %q", evA)
	}

	// A unix socket address caps at ~104 bytes, and how much of that budget the
	// home directory eats is not ours to control. What hashing guarantees — and
	// what this pins — is that the agent's NAME contributes a constant, so a
	// verbose name cannot be what pushes the path over. Blowing the cap degrades
	// steering to a polling file channel without saying so.
	long, _ := sessionSock(strings.Repeat("verbose-agent-name", 12))
	if len(filepath.Base(long)) != len(filepath.Base(sockA)) {
		t.Errorf("socket filename grew with the agent name: %q vs %q",
			filepath.Base(long), filepath.Base(sockA))
	}
}

func TestPublishSessionTTYCreatesBashyOwnedLine(t *testing.T) {
	isolateHome(t)
	realTTY := filepath.Join(t.TempDir(), "real-tty")
	if err := os.WriteFile(realTTY, nil, 0o620); err != nil {
		t.Fatal(err)
	}
	line, link, err := publishSessionTTY("elif", realTTY)
	if err != nil {
		t.Fatal(err)
	}
	if line != "elif" {
		t.Fatalf("line = %q, want agent id", line)
	}
	if filepath.Dir(link) != whodb.PTYDirForEnv(os.Environ()) {
		t.Fatalf("link = %q, want under bashy pty dir %q", link, whodb.PTYDirForEnv(os.Environ()))
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != realTTY {
		t.Fatalf("pty link points at %q, want %q", target, realTTY)
	}
}

// TestCardEventsFilePrefersWhatTheCardAdvertises — a card publishes its own
// event stream, so no reader recomputes a hash and the two cannot drift.
func TestCardEventsFilePrefersWhatTheCardAdvertises(t *testing.T) {
	isolateHome(t)
	card := room.Card{
		ID: "elif", Binding: "ycode:glm-5.2", PID: os.Getpid(),
		Events: true, EventsPath: "/tmp/advertised.ndjson",
	}
	if got := cardEventsFile(card); got != "/tmp/advertised.ndjson" {
		t.Fatalf("cardEventsFile = %q, want the advertised path", got)
	}
}

// TestCardEventsFileFallsBackForLegacyCard — a card written before EventsPath
// existed still resolves, to exactly the path it had when it was written.
func TestCardEventsFileFallsBackForLegacyCard(t *testing.T) {
	isolateHome(t)
	legacy := room.Card{ID: "claude-4242", Binding: "claude:fable5", PID: 4242, Events: true}
	got := cardEventsFile(legacy)
	if got == "" {
		t.Fatal("legacy card resolved to no events path")
	}
	want := shortHash(legacy.Binding + "\x00" + "4242")
	if !strings.Contains(got, want) {
		t.Fatalf("cardEventsFile = %q, want the pre-EventsPath binding+pid key %q", got, want)
	}
}

// TestErrAgentLiveNamesBothWaysForward — a refusal with no next step is how an
// operator learns to reach for --force. Both exits must be in the message.
func TestErrAgentLiveNamesBothWaysForward(t *testing.T) {
	msg := errAgentLive("elif", 4242, "/src/coreutils").Error()
	for _, want := range []string{"elif", "4242", "/src/coreutils", "--attach", "agent clone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}
}

// TestRefuseIfSessionLiveOnlyGatesStoreLockingTools — the guard exists because
// a ycode turn fired at an agent mid-session dies inside the tool with
// "storage is locked", naming neither the agent nor the holder. It must NOT
// grow into a general ban on using a name twice: a tool without a locked store
// runs a turn beside a session perfectly well.
func TestRefuseIfSessionLiveOnlyGatesStoreLockingTools(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	// A live session held by SOME OTHER process (our parent is alive by
	// construction), under the identity both launches below resolve to.
	if err := room.Join(room.Card{
		ID: "elif", Nick: "elif", Binding: "ycode:glm-5.2", Tool: "ycode",
		Mode: "interactive", PID: os.Getppid(), Cwd: "/src/coreutils",
	}); err != nil {
		t.Fatal(err)
	}

	ycode := Launch{Nick: "elif", ToolName: "ycode", ModelName: "glm-5.2"}
	err := refuseIfSessionLive(ycode)
	if err == nil {
		t.Fatal("a ycode turn against a live session must be refused in our own words")
	}
	if !strings.Contains(err.Error(), "agent clone") {
		t.Errorf("refusal should name the way forward, got: %v", err)
	}

	// Same identity, a tool that does not lock a store: allowed.
	other := Launch{Nick: "elif", ToolName: "codex", ModelName: "gpt-5.5"}
	if err := refuseIfSessionLive(other); err != nil {
		t.Errorf("a non-store-locking tool must not be gated, got: %v", err)
	}
}

// TestRefuseIfSessionLiveIgnoresOurOwnSession — a session steering itself
// (coach, foreman) is not two processes racing a store, and must not be refused.
func TestRefuseIfSessionLiveIgnoresOurOwnSession(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := room.Join(room.Card{
		ID: "elif", Nick: "elif", Binding: "ycode:glm-5.2", Tool: "ycode",
		Mode: "interactive", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := refuseIfSessionLive(Launch{Nick: "elif", ToolName: "ycode", ModelName: "glm-5.2"}); err != nil {
		t.Errorf("our own session must not refuse our own turn, got: %v", err)
	}
}
