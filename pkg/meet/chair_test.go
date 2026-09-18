package meet

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seqRunner replies from a per-agent script, advancing one entry per call, so a
// chair can be made to answer differently on successive turns. Past the end
// of a script the last entry repeats — which is what a looping meeting looks like.
type seqRunner struct {
	scripts map[string][]string
	calls   map[string]int
	err     map[string]error
}

func newSeqRunner() *seqRunner {
	return &seqRunner{scripts: map[string][]string{}, calls: map[string]int{}, err: map[string]error{}}
}

func (s *seqRunner) Run(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
	if e := s.err[agent]; e != nil {
		return "", 1, e
	}
	script := s.scripts[agent]
	i := s.calls[agent]
	s.calls[agent]++
	if len(script) == 0 {
		return "contribution from " + agent, 0, nil
	}
	if i >= len(script) {
		i = len(script) - 1
	}
	return script[i], 0, nil
}

func ledger(satisfied, looping, progressing bool, next string) string {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return "SATISFIED: " + yn(satisfied) + "\nLOOPING: " + yn(looping) +
		"\nPROGRESSING: " + yn(progressing) + "\nNEXT: " + next +
		"\nINSTRUCTION: address the open point\nREASON: because"
}

func chairedSession(t *testing.T) *State {
	t.Helper()
	st := newTestSession(t)
	st.Chair = "chairbot" // distinct from the secretary and from every participant
	st.MaxTurns = 6
	st.MaxStalls = 3
	_ = st.save()
	return st
}

func TestParseLedger(t *testing.T) {
	l := parseLedger("SATISFIED: no\nLOOPING: YES\nPROGRESSING: no\nNEXT: **codex**\nINSTRUCTION: do x\nREASON: because y")
	if l.Satisfied || !l.Looping || l.Progressing {
		t.Errorf("flags: %+v", l)
	}
	if l.NextSpeaker != "codex" || l.Instruction != "do x" || l.Reason != "because y" {
		t.Errorf("fields: %+v", l)
	}
	if !l.stalling() {
		t.Error("looping + no progress must count as stalling")
	}

	// A garbled reply must never end the meeting nor fake a stall: the backstops
	// bound the loop, not the parse.
	g := parseLedger("I think codex should go next, honestly.")
	if g.Satisfied {
		t.Error("an unparseable reply must not report the request satisfied")
	}
	if g.stalling() {
		t.Error("an unparseable reply must not be read as a stall")
	}
	if g.NextSpeaker != "" {
		t.Errorf("no NEXT line should yield no speaker, got %q", g.NextSpeaker)
	}
}

// Exact match only. Loose matching is what makes `code-reviewer` silently route
// to `reviewer`.
func TestResolveSpeakerIsExact(t *testing.T) {
	roster := []string{"codex", "code-reviewer"}
	for _, in := range []string{"codex", "CODEX", " codex "} {
		if got, ok := resolveSpeaker(in, roster); !ok || got != "codex" {
			t.Errorf("resolveSpeaker(%q) = %q,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "reviewer", "code", "codex and claude", "claude"} {
		if got, ok := resolveSpeaker(in, roster); ok {
			t.Errorf("resolveSpeaker(%q) should fail, got %q", in, got)
		}
	}
}

// The validation ladder: a chair that names a nonexistent participant is
// re-prompted, and a valid retry is accepted.
func TestSelectorLadderRepromptsThenAccepts(t *testing.T) {
	st := chairedSession(t)
	r := newSeqRunner()
	r.scripts["chairbot"] = []string{
		ledger(false, false, true, "reviewer"), // not on the roster
		ledger(false, false, true, "codex"),    // valid
	}
	l := nextLedger(context.Background(), st, st.Participants, "opencode", r)
	if l.Degraded {
		t.Fatalf("a valid retry must not degrade: %+v", l)
	}
	if l.NextSpeaker != "codex" {
		t.Errorf("next = %q, want codex", l.NextSpeaker)
	}
	if r.calls["chairbot"] != 2 {
		t.Errorf("expected exactly one re-prompt, chair called %d times", r.calls["chairbot"])
	}
}

// After the ladder is exhausted, degrade to a default speaker — and SAY SO. A
// silent fallback looks like a working selector.
func TestSelectorLadderDegradesLoudly(t *testing.T) {
	st := chairedSession(t)
	r := newSeqRunner()
	r.scripts["chairbot"] = []string{ledger(false, false, true, "nobody")}

	l := nextLedger(context.Background(), st, st.Participants, "opencode", r)
	if !l.Degraded {
		t.Fatal("exhausting the ladder must mark the ledger degraded")
	}
	if l.NextSpeaker != "opencode" {
		t.Errorf("next = %q, want the fallback opencode", l.NextSpeaker)
	}
	if !strings.Contains(l.Reason, "failed to name a valid participant") {
		t.Errorf("degradation must explain itself: %q", l.Reason)
	}
	if r.calls["chairbot"] != maxSelectorAttempts {
		t.Errorf("ladder ran %d times, want %d", r.calls["chairbot"], maxSelectorAttempts)
	}
	// A dead chair degrades too, rather than hanging or routing nowhere.
	st2 := chairedSession(t)
	dead := newSeqRunner()
	dead.err["chairbot"] = errors.New("boom")
	if l2 := nextLedger(context.Background(), st2, st2.Participants, "codex", dead); !l2.Degraded || l2.NextSpeaker != "codex" {
		t.Errorf("dead chair should degrade to the fallback, got %+v", l2)
	}
}

// `satisfied` ends the loop, and it is the ONLY outcome the chair chooses.
func TestChairedStopsWhenSatisfied(t *testing.T) {
	st := chairedSession(t)
	r := newSeqRunner()
	r.scripts["chairbot"] = []string{
		ledger(false, false, true, "codex"),
		ledger(true, false, true, "codex"), // satisfied on the second ledger
	}
	res, err := runChaired(context.Background(), st, r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Satisfied || res.StoppedBy != "satisfied" {
		t.Fatalf("res = %+v", res)
	}
	if res.Turns != 1 {
		t.Errorf("one participant turn should have run, got %d", res.Turns)
	}
	// Only the selected participant spoke — that is the point of chairing.
	events, _ := readTranscript(st.ID)
	for _, e := range events {
		if e.Kind == "turn" && e.Speaker != "codex" {
			t.Errorf("unselected participant %s spoke", e.Speaker)
		}
	}
	if countKind(events, "ledger") != 2 {
		t.Errorf("every chair decision must be recorded, got %d ledgers", countKind(events, "ledger"))
	}
}

// An agent that never says "satisfied" must not run forever. Termination is the
// orchestrator's, never the model's.
func TestChairedHonorsMaxTurns(t *testing.T) {
	st := chairedSession(t)
	st.MaxTurns = 3
	_ = st.save()
	r := newSeqRunner()
	r.scripts["chairbot"] = []string{ledger(false, false, true, "codex")} // never satisfied

	res, err := runChaired(context.Background(), st, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 3 || res.StoppedBy != "max_turns" {
		t.Fatalf("res = %+v, want 3 turns stopped by max_turns", res)
	}
	if res.Satisfied {
		t.Error("a backstop stop must not report the request satisfied")
	}
}

// The stall counter: a looping meeting triggers a re-plan rather than calling on
// yet another participant to repeat the loop.
func TestChairedStallTriggersReplanThenGivesUp(t *testing.T) {
	st := chairedSession(t)
	st.MaxTurns = 20
	st.MaxStalls = 2
	_ = st.save()
	r := newSeqRunner()
	// Every ledger reports looping + no progress; the replan reply is prose.
	r.scripts["chairbot"] = []string{ledger(false, true, false, "codex")}

	res, err := runChaired(context.Background(), st, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedBy != "stalled" {
		t.Fatalf("a permanently looping meeting must stop as stalled, got %+v", res)
	}
	if res.Replans < 2 {
		t.Errorf("expected the re-plan escape to fire before giving up, got %d", res.Replans)
	}
	// Critically: it must NOT have burned all 20 turns dispatching participants.
	if res.Turns >= 20 {
		t.Errorf("stall detection must precede dispatch; ran %d turns", res.Turns)
	}
	events, _ := readTranscript(st.ID)
	if countKind(events, "replan") == 0 {
		t.Error("the new approach must be recorded in the transcript")
	}
}

// A cancelled context stops the loop promptly and says so.
func TestChairedHonorsDeadline(t *testing.T) {
	st := chairedSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := runChaired(ctx, st, newSeqRunner())
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedBy != "deadline" || res.Satisfied {
		t.Fatalf("res = %+v", res)
	}
}

func TestChairedNeedsParticipants(t *testing.T) {
	st := newTestSession(t)
	st.Participants = nil
	if _, err := runChaired(context.Background(), st, newSeqRunner()); err == nil {
		t.Fatal("a chaired meeting with no participants must error, not hang")
	}
}

// The turn model is a CONSEQUENCE of who chairs, never a separate flag that could
// contradict the roster.
// seatAnyAgent relaxes the host-routability gate for tests whose subject is the
// turn model or role separation rather than whether this machine can drive a
// seat. The gate is indirected for exactly this reason; without it these tests
// depend on which agent CLIs happen to be installed, which is not what they are
// asserting. (They previously passed by accident: `meet start` did not check
// routability at all, so an unroutable seat was silently accepted.)
func seatAnyAgent(t *testing.T) {
	t.Helper()
	old := operableFn
	operableFn = func(string) (bool, string) { return true, "test" }
	t.Cleanup(func() { operableFn = old })
}

func TestTurnModelFollowsFromTheRoster(t *testing.T) {
	seatAnyAgent(t)
	plain, err := (&sessionFlags{topic: "t", secretary: "claude", participants: []string{"codex"}}).newState()
	if err != nil {
		t.Fatal(err)
	}
	if plain.chaired() {
		t.Error("no --owner facilitator must mean round-robin")
	}
	if !strings.Contains(plain.turnModel(), "round-robin") {
		t.Errorf("turnModel = %q", plain.turnModel())
	}

	chaired, err := (&sessionFlags{topic: "t", secretary: "claude", chair: "gemini",
		participants: []string{"codex"}}).newState()
	if err != nil {
		t.Fatal(err)
	}
	if !chaired.chaired() || chaired.chair() != "gemini" {
		t.Errorf("a --owner facilitator must imply the chaired turn model: %+v", chaired)
	}
	if _, err := (&sessionFlags{topic: "t", secretary: "claude", chair: "gemini"}).newState(); err == nil {
		t.Error("a chair with no participants to call on must be rejected up front")
	}
}

// Separation of powers. Each of these is a way the design fails silently, so each
// is a hard error rather than a warning.
func TestRoleSeparationIsEnforced(t *testing.T) {
	seatAnyAgent(t)
	cases := []struct {
		name string
		sf   sessionFlags
		want string
	}{
		{"secretary is also a participant",
			sessionFlags{topic: "t", secretary: "claude", participants: []string{"claude", "codex"}},
			"secretary and participant"},
		{"secretary is also the chair",
			sessionFlags{topic: "t", secretary: "claude", chair: "claude", participants: []string{"codex"}},
			"facilitator and secretary"},
		{"chair is also a participant",
			sessionFlags{topic: "t", secretary: "claude", chair: "codex", participants: []string{"codex"}},
			"facilitator and participant"},
		{"a participant is seated twice",
			sessionFlags{topic: "t", secretary: "claude", participants: []string{"codex", "codex"}},
			"seated twice"},
		{"no topic",
			sessionFlags{topic: "", secretary: "claude", participants: []string{"codex"}},
			"needs a --topic"},
	}
	for _, c := range cases {
		_, err := c.sf.newState()
		if err == nil {
			t.Errorf("%s: expected a hard error, got none", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q should explain %q", c.name, err.Error(), c.want)
		}
	}

	// The legal roster: three distinct agents in three distinct roles.
	if _, err := (&sessionFlags{topic: "t", secretary: "claude", chair: "gemini",
		participants: []string{"codex", "opencode"}}).newState(); err != nil {
		t.Fatalf("a well-separated roster must be accepted: %v", err)
	}

	// And the smallest legal roster: no secretary at all. Separation of powers
	// governs agents that hold two roles at once — it has nothing to say about a
	// role nobody holds, and a room with no recorder is a conversation, not a
	// malformed meeting.
	st, err := (&sessionFlags{topic: "t", secretary: "", participants: []string{"codex"}}).newState()
	if err != nil {
		t.Fatalf("a room with no secretary must be accepted: %v", err)
	}
	if st.recorded() {
		t.Errorf("secretary = %q, want none", st.Secretary)
	}
}
