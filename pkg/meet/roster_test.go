package meet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func ambiguousRosterFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `agents:
  - name: duplicate-owner
    tool: codex
    model: one
  - name: DUPLICATE-OWNER
    tool: claude
    model: two
  - name: alpha-owner
    aliases: [shared-owner]
    tool: codex
    model: one
  - name: beta-owner
    aliases: [shared-owner]
    tool: claude
    model: two
  - name: participant
    tool: codex
    model: one
`
	if err := os.WriteFile(filepath.Join(dir, "ambiguous.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
}

func TestMeetingFacilitatorRejectsAmbiguousAgentIdentity(t *testing.T) {
	cat := ambiguousRosterFleet(t)
	previous := registeredAgentFn
	registeredAgentFn = cat.Agent
	t.Cleanup(func() { registeredAgentFn = previous })

	for _, owner := range []string{"duplicate-owner", "shared-owner"} {
		t.Run(owner, func(t *testing.T) {
			_, err := (&sessionFlags{
				topic: "identity boundary", participants: []string{"participant"},
				secretary: "", chair: owner,
			}).newState()
			if err == nil || !strings.Contains(err.Error(), "not a registered agent") {
				t.Fatalf("ambiguous facilitator %q was not rejected: %v", owner, err)
			}
		})
	}
}

// rosterFleet builds a small catalog spanning three bands, with one L4 agent
// whose harness is not installed on this host.
func rosterFleet(t *testing.T) *fleet.Catalog {
	t.Helper()
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	for _, m := range []fleet.Model{
		{Name: "big", Band: 4},
		{Name: "mid", Band: 3},
		{Name: "small", Band: 1},
	} {
		if err := c.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"here", "gone"} {
		if err := c.SaveTool(fleet.Tool{Name: tool, Kind: fleet.ToolKindCLI}); err != nil {
			t.Fatal(err)
		}
	}
	agents := []fleet.Agent{
		{Name: "here-big", Tool: "here", Model: "big", Ledger: &fleet.AgentLedger{Reliability: "high"}},
		{Name: "here-mid", Tool: "here", Model: "mid"},
		{Name: "here-small", Tool: "here", Model: "small"},
		{Name: "gone-big", Tool: "gone", Model: "big"},
	}
	for _, a := range agents {
		if err := c.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// operability is faked so the test does not depend on what happens to be on
// this machine's PATH.
func fakeOperable(tool string) (bool, string) {
	if tool == "gone" {
		return false, "not installed"
	}
	return true, "drivable"
}

func TestSeatByBandSelectsAndSorts(t *testing.T) {
	seats, skips := SeatByBand(rosterFleet(t), 3, fakeOperable)

	if len(seats) != 2 {
		t.Fatalf("want 2 seats at L3+, got %d: %+v", len(seats), seats)
	}
	// Strongest first, so trimming a too-large roster loses the least.
	if seats[0].Agent != "here-big" || seats[0].Band != 4 {
		t.Errorf("seat 0 = %+v, want the L4 agent first", seats[0])
	}
	if seats[1].Agent != "here-mid" || seats[1].Band != 3 {
		t.Errorf("seat 1 = %+v, want the L3 agent second", seats[1])
	}
	if seats[0].Binding != "here:big" {
		t.Errorf("a seat must carry the canonical binding, got %q", seats[0].Binding)
	}
	if seats[0].Reliability != "high" {
		t.Errorf("reliability = %q, want high", seats[0].Reliability)
	}
	if seats[0].Nick == "" {
		t.Error("a seat must carry a nickname")
	}

	// The L1 agent is simply below the bar; the L4 one was selected and then
	// dropped for being unreachable. Only the second is a skip — and it must
	// be REPORTED, not swallowed, or the minutes imply a table that never sat.
	if len(skips) != 1 || skips[0].Agent != "gone-big" || skips[0].Band != 4 {
		t.Fatalf("want gone-big reported as skipped, got %+v", skips)
	}
	if skips[0].Reason == "" {
		t.Error("a skip must say why")
	}
}

// A band below everything seats everything operable; a band above everything
// seats nothing. Neither should panic or quietly return a partial roster.
func TestSeatByBandEdges(t *testing.T) {
	cat := rosterFleet(t)

	seats, _ := SeatByBand(cat, 1, fakeOperable)
	if len(seats) != 3 {
		t.Errorf("L1+ should seat all 3 operable agents, got %d", len(seats))
	}
	seats, skips := SeatByBand(cat, 4, fakeOperable)
	if len(seats) != 1 || len(skips) != 1 {
		t.Errorf("L4 = 1 seat + 1 skip, got %d seats %d skips", len(seats), len(skips))
	}
}

func TestSeatByBandSkipsDirectProviderHarnessWithoutItsKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "available")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	tool := fleet.Tool{
		Name: "direct", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{
			Exec: "direct --model {model} {prompt}", Credential: fleet.ToolCredentialModelProvider,
		}},
	}
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	for _, m := range []fleet.Model{
		{Name: "openai-frontier", Provider: "openai", Kind: fleet.ModelKindSubscription, Band: 4},
		{Name: "anthropic-frontier", Provider: "anthropic", Kind: fleet.ModelKindSubscription, Band: 4},
	} {
		if err := cat.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []fleet.Agent{
		{Name: "openai-seat", Tool: "direct", Model: "openai-frontier"},
		{Name: "anthropic-seat", Tool: "direct", Model: "anthropic-frontier"},
	} {
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}

	seats, skips := SeatByBand(cat, 4, func(string) (bool, string) { return true, "installed" })
	if len(seats) != 1 || seats[0].Agent != "openai-seat" {
		t.Fatalf("seats = %+v, want only openai-seat", seats)
	}
	if len(skips) != 1 || skips[0].Agent != "anthropic-seat" ||
		!strings.Contains(skips[0].Reason, "anthropic") {
		t.Fatalf("skips = %+v, want missing anthropic credential", skips)
	}
}

// pinFleet points the catalog at an empty local ring, so a test sees the
// shipped baseline and nothing an operator happens to have installed. It
// returns the nickname the baseline drew for an agent — looked up rather than
// hardcoded, because the assertion here is "the nickname canonicalizes", not
// "the nickname is Sable".
func pinFleet(t *testing.T) func(string) string {
	t.Helper()
	fleettest.Ring(t)
	return func(agent string) string {
		a, ok := fleet.New().Agent(agent)
		if !ok {
			t.Fatalf("test ring has no agent %q", agent)
		}
		if a.NickName() == "" {
			t.Fatalf("%s drew no nickname", agent)
		}
		return a.NickName()
	}
}

// A seat name is stamped onto every Event as its Speaker and lands in the
// minutes. Typing an alias must be free; STORING one must be impossible —
// `claude-opus` re-points when a newer Opus ships, and minutes that recorded it
// would silently re-attribute what was said to a model that never said it.
func TestRosterIsCanonicalizedBeforeItIsRecorded(t *testing.T) {
	nickOf := pinFleet(t)

	sf := &sessionFlags{
		// Every way a human might name a seat: a nickname, a floating family
		// alias, and a binding spelled with the family alias.
		participants: []string{nickOf("claude-fable5"), "claude-opus", "claude:opus"},
		secretary:    nickOf("claude-opus4.8"),
		chair:        "claude-fable",
	}
	sf.canonicalizeRoster()

	want := []string{"claude-fable5", "claude-opus5", "claude-opus5"}
	for i, got := range sf.participants {
		if got != want[i] {
			t.Errorf("participant %d = %q, want the canonical %q", i, got, want[i])
		}
	}
	if sf.secretary != "claude-opus4.8" {
		t.Errorf("secretary = %q, want claude-opus4.8", sf.secretary)
	}
	if sf.chair != "claude-fable5" {
		t.Errorf("chair = %q, want claude-fable5", sf.chair)
	}
}

// Canonicalization does not itself validate. A bare tool remains untouched so
// the catalog-only roster gate can reject it with a useful registration error.
func TestCanonicalizeLeavesUnboundNamesAlone(t *testing.T) {
	pinFleet(t)
	sf := &sessionFlags{participants: []string{"claude"}, secretary: "claude"}
	sf.canonicalizeRoster()
	if sf.participants[0] != "claude" || sf.secretary != "claude" {
		t.Errorf("a bare tool must survive untouched: %+v", sf)
	}
}

// Canonicalizing makes the duplicate check see through aliases: one agent
// seated twice under two names dilutes the vote while looking like diversity.
func TestAliasedDuplicateSeatIsCaught(t *testing.T) {
	nickOf := pinFleet(t)
	sf := &sessionFlags{
		topic:        "x",
		secretary:    "claude",
		participants: []string{nickOf("claude-fable5"), "claude-fable5"},
	}
	if _, err := sf.newState(); err == nil {
		t.Fatal("the same agent seated under two names must be rejected as a duplicate")
	}
}

func TestSeatByBandRejectsBadInput(t *testing.T) {
	sf := &sessionFlags{minBand: 9}
	if err := sf.seatByBand(); err == nil {
		t.Error("--min-band 9 must be rejected")
	}
	sf = &sessionFlags{minBand: 3, participants: []string{"someone"}}
	if err := sf.seatByBand(); err == nil {
		t.Error("--min-band with an explicit --participant must be rejected as ambiguous")
	}
	sf = &sessionFlags{} // no band: seating is a no-op, not an error
	if err := sf.seatByBand(); err != nil {
		t.Errorf("no --min-band must be a no-op: %v", err)
	}
}

// A meeting must work OUT OF THE BOX on an ordinary, uncontained host — with
// the launch guard armed and nobody having set BASHY_ALLOW_UNSAFE_AGENT_LAUNCH.
//
// It does, because no seat at a meeting needs write authority: every attendee
// produces text, which meet captures and writes itself. Launching read-only
// REMOVES the approval-gate kill-switches rather than asking permission to keep
// them, so the guard passes by construction — nothing is bypassed. Drop ReadOnly
// from invokeAgent to "make an attendee more capable" and every claude turn
// starts failing with `refusing to launch`; this test says so first.
//
// Note this goes through the real resolve path, NOT chat.Invoke --dry-run: a
// dry run deliberately skips the guard (it shows what WOULD run), so testing
// through it would pass whether or not the guard was satisfied.
func launchForMeeting(t *testing.T, tool string, readOnly bool) (agentlaunch.Launch, error) {
	t.Helper()
	return agentlaunch.ResolveWithCatalog(tool,
		agentlaunch.Options{ReadOnly: readOnly},
		func() *fleet.Catalog { return fleet.New() })
}

func TestMeetingAgentsLaunchWithoutWeakeningTheHost(t *testing.T) {
	t.Setenv(chat.UnsafeLaunchEnv, "") // an ordinary host: the guard is armed
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())

	for _, tool := range []string{"claude", "codex", "aider", "opencode"} {
		l, err := launchForMeeting(t, tool, true) // exactly what invokeAgent passes
		if err != nil {
			t.Errorf("%s: a meeting seat must launch on an uncontained host: %v", tool, err)
			continue
		}
		for _, a := range l.Args {
			if _, unsafe := agentlaunch.UnsafeLaunchFlags[a]; unsafe {
				t.Errorf("%s: a meeting seat was handed %q — an attendee argues, it does not edit",
					tool, a)
			}
		}
	}
}

// And the converse, so the guard itself cannot quietly rot: WITHOUT read-only,
// the same launch on the same host is refused. If this ever stops failing, the
// gate has been defeated, and meet's read-only launch is no longer what is
// keeping an uncontained host safe — it is just decoration.
func TestTheGuardIsRealWithoutReadOnly(t *testing.T) {
	t.Setenv(chat.UnsafeLaunchEnv, "")
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())

	if _, err := launchForMeeting(t, "claude", false); err == nil {
		t.Fatal("an uncontained host must refuse to strip claude's approval gate")
	}
}

// THE GATE THAT WAS ONLY ON ONE PATH.
//
// routableSeat existed and was reachable from inviteTo alone, so `meet invite`
// refused a name this host cannot drive while `meet start --participant`
// accepted anything. A live conductor room on this host was created that way,
// with `root-supervisor` as its sole participant: it reported `open` forever at
// round 0 with no contribution, and a message sent into it was accepted and read
// by nobody.
func TestStart_RefusesAnUnroutableSeat(t *testing.T) {
	for _, c := range []struct {
		name string
		sf   sessionFlags
	}{
		{"participant", sessionFlags{topic: "t", participants: []string{"root-supervisor"}}},
		{"secretary", sessionFlags{topic: "t", secretary: "root-supervisor", participants: []string{"claude"}}},
		{"chair", sessionFlags{topic: "t", chair: "root-supervisor", participants: []string{"claude"}}},
	} {
		_, err := c.sf.newState()
		if err == nil {
			t.Errorf("%s: an unroutable seat must be refused at creation, not seated to fail every round", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "not a registered agent") {
			t.Errorf("%s: the refusal should say why: %v", c.name, err)
		}
	}
}

func TestStart_RefusesBareInstalledToolWithoutAgentRegistration(t *testing.T) {
	pinFleet(t)
	old := operableFn
	operableFn = func(string) (bool, string) { return true, "installed" }
	t.Cleanup(func() { operableFn = old })

	_, err := (&sessionFlags{topic: "t", participants: []string{"some-installed-tool"}}).newState()
	if err == nil || !strings.Contains(err.Error(), "bashy agent list") || strings.Contains(err.Error(), "--all") {
		t.Fatalf("installed but unregistered tool must be rejected with registration guidance: %v", err)
	}
}

// The human is not an agent and is not checked: Human is its own field on
// State, and a person is not something this host drives.
func TestStart_DoesNotGateTheHumanSeat(t *testing.T) {
	seatAnyAgent(t)
	if _, err := (&sessionFlags{topic: "t", human: "some-person", participants: []string{"claude"}}).newState(); err != nil {
		t.Fatalf("a human seat must not be held to agent routability: %v", err)
	}
}
