package meet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptRunner returns a canned reply per agent, so a meeting's participants can
// disagree, crash, or go quiet independently — which is the whole point of the
// behavior under test.
type scriptRunner struct {
	replies map[string]string
	codes   map[string]int
	errs    map[string]error
}

func (s scriptRunner) Run(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
	return s.replies[agent], s.codes[agent], s.errs[agent]
}

func TestNormalizeChoice(t *testing.T) {
	choices := []string{"yes", "no", "abstain"}
	cases := []struct {
		reply, want string
	}{
		{"yes", "yes"},
		{"YES\nbecause it is safe", "yes"},
		{"**Yes** — the slicer is a no-op at n=1", "yes"},
		{"Answer: no", "no"},
		{"vote: NO\nrationale follows", "no"},
		{"- yes", "yes"},
		{"`abstain`", "abstain"},
		{"I lean yes, though no is defensible", ""}, // ambiguous: names two choices
		{"maybe", ""},                               // outside the ballot
		{"", ""},
		{"Some preamble line one.\nSome preamble two.\nyes", "yes"}, // verdict within the head window
	}
	for _, c := range cases {
		if got := normalizeChoice(c.reply, choices); got != c.want {
			t.Errorf("normalizeChoice(%q) = %q, want %q", c.reply, got, c.want)
		}
	}
}

// An agent that answers but does not answer the BALLOT is `invalid`, not `ok` —
// otherwise an unparseable reply would silently vanish from the tally.
func TestPollTalliesAndFlagsInvalid(t *testing.T) {
	st := newTestSession(t)
	runner := scriptRunner{replies: map[string]string{
		"codex":    "**yes**\nthe n=1 no-op test gates it",
		"opencode": "I could see it either way, honestly",
	}}
	res, err := runPoll(context.Background(), st, "Bypass the atomizer under cert?", nil, nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tally["yes"] != 1 {
		t.Errorf("tally[yes] = %d, want 1", res.Tally["yes"])
	}
	if res.Tally[statusInvalid] != 1 {
		t.Errorf("unparseable reply should tally as invalid, got %+v", res.Tally)
	}
	// One valid vote against one non-answer is a sample of one, not a consensus.
	if _, ok := res.Winner(); ok {
		t.Errorf("1 yes + 1 invalid must not be decisive, tally=%+v", res.Tally)
	}
	// The vote is reconstructible from the transcript alone.
	events, _ := readTranscript(st.ID)
	var polls, votes int
	for _, e := range events {
		switch e.Kind {
		case "poll":
			polls++
		case "vote":
			votes++
		}
	}
	if polls != 1 || votes != 2 {
		t.Errorf("transcript: polls=%d votes=%d, want 1 and 2", polls, votes)
	}
}

func TestPollWinnerRequiresDecisiveResult(t *testing.T) {
	choices := []string{"yes", "no"}
	cases := []struct {
		name  string
		tally map[string]int
		win   string
		ok    bool
	}{
		{"unanimous", map[string]int{"yes": 2}, "yes", true},
		{"majority over a crash", map[string]int{"yes": 2, statusError: 1}, "yes", true},
		{"tie", map[string]int{"yes": 1, "no": 1}, "", false},
		{"nobody answered", map[string]int{statusEmpty: 2}, "", false},
		{"lead does not clear the non-answers", map[string]int{"yes": 1, statusTimeout: 1}, "", false},
		{"lead swamped by non-answers", map[string]int{"yes": 2, statusInvalid: 3}, "", false},
	}
	for _, c := range cases {
		p := &PollResult{Choices: choices, Tally: c.tally}
		win, ok := p.Winner()
		if ok != c.ok || (ok && win != c.win) {
			t.Errorf("%s: Winner() = %q,%v; want %q,%v", c.name, win, ok, c.win, c.ok)
		}
	}
}

// Silence on an OPTIONAL question is a contribution ("no comment"), not a tool
// failure — it must not ding the participant's operability.
func TestAskOptionalSilenceIsAbstentionNotFailure(t *testing.T) {
	st := newTestSession(t)
	runner := scriptRunner{replies: map[string]string{
		"codex":    "(no comment)",
		"opencode": "", // said nothing at all
	}}
	evs, err := runAsk(context.Background(), st, "Anything else?", true, nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if got := statusOf(e); got != statusAbstain {
			t.Errorf("%s status = %q, want abstain", e.Speaker, got)
		}
		if turnFailed(e) {
			t.Errorf("%s: an abstention must not count as a failed turn", e.Speaker)
		}
	}
	// Required mode: the same silence IS a failure.
	st2 := newTestSession(t)
	evs2, err := runAsk(context.Background(), st2, "Anything else?", false, []string{"opencode"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if statusOf(evs2[0]) != statusEmpty || !turnFailed(evs2[0]) {
		t.Errorf("required question: empty reply must be a failure, got %q", statusOf(evs2[0]))
	}
}

// "opencode returned no content" is useless; an operator needs to know whether it
// crashed, hung, or thought and said nothing.
func TestTurnStatusDistinguishesFailureModes(t *testing.T) {
	st := newTestSession(t)
	st.MinTurnChars = 20
	st.Participants = []string{"crasher", "quiet", "terse", "slow"}
	runner := scriptRunner{
		replies: map[string]string{"crasher": "boom", "quiet": "", "terse": "ok", "slow": ""},
		codes:   map[string]int{"crasher": 3},
		errs: map[string]error{
			"crasher": errors.New("exit status 3"),
			"slow":    context.DeadlineExceeded,
		},
	}
	want := map[string]string{
		"crasher": statusError, "quiet": statusEmpty, "terse": statusShort, "slow": statusTimeout,
	}
	evs, err := runRound(context.Background(), st, "q", runner)
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	for _, e := range evs {
		if got := statusOf(e); got != want[e.Speaker] {
			t.Errorf("%s status = %q, want %q", e.Speaker, got, want[e.Speaker])
		}
	}

	events, _ := readTranscript(st.ID)
	rows := coverage(st, events)
	byName := map[string]Coverage{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if c := byName["crasher"]; c.Errors != 1 || c.ExitCode != 3 || !c.RetryRecommended() {
		t.Errorf("crasher coverage = %+v; want 1 error, exit 3, retry recommended", c)
	}
	if c := byName["quiet"]; c.Empty != 1 || c.RetryRecommended() {
		t.Errorf("quiet coverage = %+v; an empty reply is not worth retrying", c)
	}
	if c := byName["slow"]; c.Timeout != 1 || !c.RetryRecommended() {
		t.Errorf("slow coverage = %+v; a timeout is worth retrying", c)
	}
	if c := byName["terse"]; c.Short != 1 || c.Contributed() {
		t.Errorf("terse coverage = %+v; a 2-char reply is not a contribution", c)
	}
}

// The secretary's synthesis lives outside the append-only transcript, so
// re-running it rewrites rather than duplicates. Before this, a second converge
// appended a second set of decision markers and the minutes double-counted.
func TestConvergeIsIdempotentAndNeverAppendsMarkers(t *testing.T) {
	st := newTestSession(t)
	reply := "DECISIONS:\n- ship it\nACTIONS:\n- claude: file it\nSUMMARY:\nAgreed."
	runner := scriptRunner{replies: map[string]string{"claude": reply}}

	for i := 0; i < 3; i++ {
		syn, err := converge(context.Background(), st, runner)
		if err != nil {
			t.Fatal(err)
		}
		if len(syn.Decisions) != 1 || len(syn.Actions) != 1 {
			t.Fatalf("pass %d: dec=%d act=%d", i, len(syn.Decisions), len(syn.Actions))
		}
	}
	events, _ := readTranscript(st.ID)
	for _, e := range events {
		if e.Kind == "decision" || e.Kind == "action" {
			t.Fatalf("secretary must not append markers to the transcript, found %+v", e)
		}
	}
	if syn := loadSynthesis(st.ID); syn == nil || len(syn.Decisions) != 1 {
		t.Fatalf("synthesis.json should hold exactly one decision, got %+v", syn)
	}
}

// The grounding contract. Dialogue summarizers invent "decisions that were
// implied but never made" at a measured ~23%; an inferred decision that cannot
// name a proposer AND an acceptor is demoted to an open question, in code.
func TestInferredDecisionsNeedNamedAgreement(t *testing.T) {
	syn := parseConverge("DECISIONS:\n" +
		"- cert rejects --chunks\n" + // stated outright: needs no support
		"- (inferred) the fd race blocks the cert [agreed: codex, claude]\n" + // proposal + acceptance
		"- (inferred) we should also chunk zsh [agreed: codex]\n" + // proposal only
		"- (inferred) somebody mentioned arm64\n") // nothing at all
	syn.demoteUnsupported()

	if len(syn.Decisions) != 2 {
		t.Fatalf("want 2 surviving decisions, got %d: %+v", len(syn.Decisions), syn.Decisions)
	}
	if syn.Decisions[0].Inferred || syn.Decisions[0].Text != "cert rejects --chunks" {
		t.Errorf("stated decision mangled: %+v", syn.Decisions[0])
	}
	d := syn.Decisions[1]
	if !d.Inferred || d.Text != "the fd race blocks the cert" || len(d.Support) != 2 {
		t.Errorf("supported inference mis-parsed: %+v", d)
	}
	if len(syn.OpenQuestions) != 2 {
		t.Fatalf("both under-supported inferences must become open questions, got %v", syn.OpenQuestions)
	}
	for _, q := range syn.OpenQuestions {
		if !strings.Contains(q, "no recorded agreement") {
			t.Errorf("demoted item should say why: %q", q)
		}
	}
}

// The agent-callable contract: disagreement is a first-class outcome, and a
// half-answering panel never reports `agree` however loudly the answering half
// agreed.
func TestVerdictDecide(t *testing.T) {
	ok := Coverage{Name: "a", Turns: 1, OK: 1}
	dead := Coverage{Name: "b", Turns: 1, Errors: 1}
	dec := []Decision{{Text: "ship"}}

	cases := []struct {
		name string
		v    Verdict
		want string
		exit int
	}{
		{"unanimous poll, full panel, no risks",
			Verdict{Coverage: []Coverage{ok, {Name: "b", Turns: 1, OK: 1}}, Decisions: dec,
				Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 2}}},
			verdictAgree, exitAgree},
		{"agreement but blocking risks raised",
			Verdict{Coverage: []Coverage{ok, {Name: "b", Turns: 1, OK: 1}}, Decisions: dec, Risks: []string{"race"},
				Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 2}}},
			verdictAgree, exitBlocked},
		{"tied poll is a split",
			Verdict{Coverage: []Coverage{ok, {Name: "b", Turns: 1, OK: 1}}, Decisions: dec,
				Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 1, "no": 1}}},
			verdictSplit, exitEscalate},
		{"half the panel crashed — never agree",
			Verdict{Coverage: []Coverage{ok, dead}, Decisions: dec,
				Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 1, statusError: 1}}},
			verdictEscalate, exitEscalate},
		{"nothing decided",
			Verdict{Coverage: []Coverage{ok}, OpenQuestions: []string{"which arch?"}},
			verdictEscalate, exitEscalate},
	}
	for _, c := range cases {
		v := c.v
		v.decide()
		if v.Verdict != c.want || v.ExitCode != c.exit {
			t.Errorf("%s: verdict=%s exit=%d; want %s %d", c.name, v.Verdict, v.ExitCode, c.want, c.exit)
		}
		if v.Confidence < 0 || v.Confidence > 1 {
			t.Errorf("%s: confidence %.2f out of range", c.name, v.Confidence)
		}
	}

	// Confidence must reflect coverage, not the panel's enthusiasm.
	full := Verdict{Coverage: []Coverage{ok, {Name: "b", Turns: 1, OK: 1}}, Decisions: dec,
		Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 2}}}
	half := Verdict{Coverage: []Coverage{ok, dead}, Decisions: dec,
		Poll: &PollResult{Choices: []string{"yes", "no"}, Tally: map[string]int{"yes": 1, statusError: 1}}}
	full.decide()
	half.decide()
	if !(full.Confidence > half.Confidence) {
		t.Errorf("a complete panel must be more confident than a half-dead one: %.2f vs %.2f",
			full.Confidence, half.Confidence)
	}
}

// An agent that convened a meeting decides when it ends. CONTINUE must block the
// close; an unparseable verdict must default to CONTINUE rather than silently
// ending someone else's meeting.
func TestAgentInitiatorGatesTheClose(t *testing.T) {
	st := newTestSession(t)
	st.Initiator = "codex" // a participant convened it, so an agent must confirm
	_ = st.save()

	declining := scriptRunner{replies: map[string]string{
		"codex":  "CONTINUE\nthe risks section is still empty",
		"claude": "SUMMARY:\nStill going.",
	}}
	_, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true, Confirm: true}, declining)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("close should be declined, got %v", err)
	}

	agreeing := scriptRunner{replies: map[string]string{
		"codex":  "CONCLUDE\nall four agenda items resolved",
		"claude": "SUMMARY:\nDone.",
	}}
	path, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true, Confirm: true}, agreeing)
	if err != nil {
		t.Fatalf("close should succeed after CONCLUDE: %v", err)
	}
	if path == "" {
		t.Fatal("no minutes written")
	}
	events, _ := readTranscript(st.ID)
	if countKind(events, "confirm") != 2 {
		t.Errorf("both confirmation attempts should be recorded, got %d", countKind(events, "confirm"))
	}
}

func TestParseVerdictDefaultsToContinue(t *testing.T) {
	if v, _ := parseVerdict("CONCLUDE\nwe are done"); v != "CONCLUDE" {
		t.Errorf("v = %q", v)
	}
	if v, _ := parseVerdict("CONTINUE\nnot yet"); v != "CONTINUE" {
		t.Errorf("v = %q", v)
	}
	// An agent that rambles has not agreed to end the meeting.
	if v, reason := parseVerdict("I think we've covered a lot of ground today"); v != "CONTINUE" || reason == "" {
		t.Errorf("unparseable verdict must default to CONTINUE with a reason, got %q %q", v, reason)
	}
}

// `--yes` is an override, not a silence: it is recorded in the transcript.
func TestYesBypassIsRecorded(t *testing.T) {
	st := newTestSession(t)
	if err := confirmConclusion(context.Background(), st, nil, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	events, _ := readTranscript(st.ID)
	if countKind(events, "confirm") != 1 {
		t.Fatal("--yes must still record a confirm event")
	}
}

// An unnamed initiator is the anonymous agent that called `meet consult`. It
// receives its verdict synchronously and never confirms — so if anything asks it
// to confirm, there is nobody to ask, and the close must refuse rather than
// invent someone.
func TestUnnamedInitiatorCannotConfirm(t *testing.T) {
	st := newTestSession(t)
	st.Initiator = ""
	_ = st.save()
	err := confirmConclusion(context.Background(), st, nil, &strings.Builder{}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "no named initiator") {
		t.Fatalf("want a no-named-initiator error, got %v", err)
	}
	// --yes still closes it, and still records that it did.
	if err := confirmConclusion(context.Background(), st, nil, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	events, _ := readTranscript(st.ID)
	if countKind(events, "confirm") != 1 {
		t.Error("--yes must record the bypass even for an unnamed caller")
	}
}

// A human initiator on a non-terminal stdin cannot be prompted; refusing beats
// silently closing their meeting.
func TestHumanConfirmRefusesWithoutTerminal(t *testing.T) {
	st := newTestSession(t)
	err := confirmConclusion(context.Background(), st, strings.NewReader("y\n"), &strings.Builder{}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want a not-a-terminal error naming --yes, got %v", err)
	}
}

// A panelist must not convene a panel: the recursion is unbounded.
func TestDepthGuardRefusesNestedMeetings(t *testing.T) {
	t.Setenv(meetDepthEnv, "")
	if err := guardDepth(); err != nil {
		t.Fatalf("top-level meeting must be allowed: %v", err)
	}
	markDepth()
	if os.Getenv(meetDepthEnv) != "1" {
		t.Fatalf("markDepth did not stamp the environment: %q", os.Getenv(meetDepthEnv))
	}
	if err := guardDepth(); err == nil {
		t.Fatal("a meeting convened from inside a meeting must be refused")
	}
}

// Agent CLIs print their workdir in startup banners, and the minutes are
// committed. Home must never reach the file.
func TestRedactHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	in := "workdir: " + filepath.Join(home, "projects", "x") + " model: gpt"
	got := redactHome(in)
	if strings.Contains(got, home) {
		t.Fatalf("home leaked: %q", got)
	}
	// ToSlash so the assertion holds on Windows, where filepath.Join (and thus
	// the redacted path) uses '\'. Redaction's job is that home doesn't leak
	// (checked above); the separator is platform-native.
	if !strings.Contains(filepath.ToSlash(got), "~/projects/x") {
		t.Fatalf("want ~-relative path, got %q", got)
	}
}

// The minutes must carry the ARGUMENT, not a 240-char ellipsis of it — that was
// the single loudest complaint about the old renderer.
func TestMinutesCarryFullTurnsCoverageAndPolls(t *testing.T) {
	st := newTestSession(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	// The room is closed from where it was opened — the new fileMinutes contract
	// refuses to file into a repo that is not the caller's.
	t.Chdir(repo)
	st.Initiator = "qiangli" // == st.Human, so a human must confirm
	_ = st.save()

	longArgument := "The slicer must be a strict no-op at n=1. " + strings.Repeat("Otherwise everything downstream is theatre. ", 12)
	runner := scriptRunner{replies: map[string]string{
		"codex":    longArgument,
		"opencode": "yes",
		"claude":   "DECISIONS:\n- (inferred) cert bypasses the atomizer [agreed: codex, opencode]\nRISKS:\n- the fd race is unfixed\nCORRECTIONS:\n- chunks=1 is not unchunked\nSUMMARY:\nAgreed.",
	}}
	runRound(context.Background(), st, "q", runner)
	if _, err := runPoll(context.Background(), st, "Ship it?", nil, nil, runner); err != nil {
		t.Fatal(err)
	}
	path, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true}, runner)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(b)
	for _, must := range []string{
		"Initiator: qiangli (human)",
		"## Summary",
		"*(inferred from consensus; agreed: codex, opencode)*", // the grounding contract
		"## Risks",                         // new section
		"## Corrections / revised framing", // stale-agenda guard
		"## Polls",                         // the tally
		"## Participant coverage",          // the failure table
		"| Participant | Turns |",          // table header
		"Otherwise everything downstream",  // the full argument, not an ellipsis
		"> The slicer must be a strict",    // rendered as a blockquote
	} {
		if !strings.Contains(md, must) {
			t.Fatalf("minutes missing %q\n---\n%s", must, md)
		}
	}
	if strings.Contains(md, "(none recorded — discussed without an explicit /decision)") {
		t.Error("a meeting that reached a consensus must not file 'none recorded'")
	}
}

// Amend regenerates the minutes from the durable transcript — the fix for a weak
// secretary pass.
func TestAmendRewritesMinutesFromTranscript(t *testing.T) {
	st := newTestSession(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo) // close from the repo the room was opened in (fileMinutes contract)
	_ = st.save()
	runRound(context.Background(), st, "q", scriptRunner{replies: map[string]string{
		"codex": "cert must bypass the atomizer", "opencode": "agreed",
	}})

	// A weak first pass records nothing.
	weak := scriptRunner{replies: map[string]string{"claude": "DECISIONS:\nnone\nSUMMARY:\nUnclear."}}
	if _, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true}, weak); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(minutesPath(st))
	if !strings.Contains(string(md), "(none — the meeting reached no decision)") {
		t.Fatal("expected the weak pass to file no decisions")
	}

	// Amend re-runs the secretary and rewrites the same file.
	strong := scriptRunner{replies: map[string]string{"claude": "DECISIONS:\n- cert bypasses the atomizer\nSUMMARY:\nClear."}}
	if _, err := converge(context.Background(), st, strong); err != nil {
		t.Fatal(err)
	}
	path, err := fileMinutes(st)
	if err != nil {
		t.Fatal(err)
	}
	md2, _ := os.ReadFile(path)
	if !strings.Contains(string(md2), "cert bypasses the atomizer") {
		t.Fatalf("amend did not pick up the new synthesis:\n%s", md2)
	}
	if strings.Contains(string(md2), "(none — the meeting reached no decision)") {
		t.Fatal("amend left the stale 'no decision' text")
	}
}

// Applying twice must not duplicate the block.
func TestApplyActionsIsIdempotent(t *testing.T) {
	st := newTestSession(t)
	_, _ = record(st, "action", "qiangli", "", "measure T and L before scheduling")
	events, _ := readTranscript(st.ID)
	syn := &Synthesis{Actions: []string{"add the reference arm", "measure T and L before scheduling"}}

	target := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(target, []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyActions(st, events, syn, target, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	got := string(b)
	if !strings.Contains(got, "- [ ] add the reference arm") {
		t.Fatalf("action block not written:\n%s", got)
	}
	if strings.Count(got, "measure T and L before scheduling") != 1 {
		t.Errorf("duplicate action item survived dedup:\n%s", got)
	}
	if _, err := applyActions(st, events, syn, target, false); err != nil {
		t.Fatal(err) // print mode never mutates
	}
	if _, err := applyActions(st, events, syn, target, true); err == nil {
		t.Fatal("a second --write must refuse rather than duplicate the block")
	}
}

// The shared source set reaches every participant: chat.BuildPrompt inlines each
// --context file, so a missing file must fail loudly at start, not mid-round.
func TestContextFilesAreValidatedUpFront(t *testing.T) {
	sf := sessionFlags{topic: "t", secretary: "claude", context: []string{filepath.Join(t.TempDir(), "nope.md")}}
	if _, err := sf.newState(); err == nil {
		t.Fatal("a missing --context file must fail before any agent is launched")
	}
}

// The initiator's kind is DERIVED from the roster, so the two can never disagree.
func TestInitiatorKindDerivedNeverStored(t *testing.T) {
	t.Setenv("USER", "alice")
	st, err := (&sessionFlags{topic: "t", secretary: "claude",
		participants: []string{"codex"}, initiator: "codex"}).newState()
	if err != nil {
		t.Fatal(err)
	}
	if st.initiatorKind() != "agent" {
		t.Errorf("a participant initiator is an agent, got %q", st.initiatorKind())
	}
	human, _ := (&sessionFlags{topic: "t", secretary: "claude",
		participants: []string{"codex"}, initiator: "alice"}).newState()
	if human.initiatorKind() != "human" {
		t.Errorf("the human initiator must be human, got %q", human.initiatorKind())
	}
	// An unnamed initiator is the calling agent (consult); it never confirms.
	caller, _ := (&sessionFlags{topic: "t", secretary: "claude", participants: []string{"codex"}}).newState()
	if caller.initiatorKind() != "agent" || caller.initiatorName() != "" {
		t.Errorf("an unnamed initiator must be an unnamed agent, got %q/%q",
			caller.initiatorName(), caller.initiatorKind())
	}
	// An initiator nobody has heard of is a mistake, not an agent.
	if _, err := (&sessionFlags{topic: "t", secretary: "claude",
		participants: []string{"codex"}, initiator: "ghost"}).newState(); err == nil {
		t.Error("an initiator who is not at the meeting must be rejected")
	}
}

// A failed secretary pass must never file minutes that READ like a finding.
// "the meeting reached no decision" is a success-shaped claim; reaching it
// because the secretary could not launch is the fleet-evidence-invariant bug
// (a state that asserts an outcome, arrived at by the ABSENCE of evidence).
func TestFailedSecretaryFilesUnknownNotNoDecision(t *testing.T) {
	st := newTestSession(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo) // close from the repo the room was opened in (fileMinutes contract)
	_ = st.save()
	runRound(context.Background(), st, "q", scriptRunner{replies: map[string]string{
		"codex": "ship the tap first", "opencode": "agreed, tap first",
	}})

	// The secretary cannot launch at all — exactly the observed failure.
	broken := scriptRunner{errs: map[string]error{"claude": errors.New("refusing to launch")}}
	if _, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true}, broken); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(minutesPath(st))
	if strings.Contains(string(md), "reached no decision") {
		t.Fatal("a failed secretary pass filed minutes claiming the meeting decided nothing")
	}
	if !strings.Contains(string(md), "UNKNOWN") {
		t.Fatalf("expected an explicit UNKNOWN notice, got:\n%s", md)
	}
	if !strings.Contains(string(md), "--resynthesize") {
		t.Fatal("the notice must name the recovery command")
	}
}

// A meeting whose secretary died is recoverable by naming another one, without
// re-running the deliberation.
func TestOverrideSecretaryRecoversSynthesis(t *testing.T) {
	st := newTestSession(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo) // close from the repo the room was opened in (fileMinutes contract)
	_ = st.save()
	runRound(context.Background(), st, "q", scriptRunner{replies: map[string]string{
		"codex": "ship the tap first", "opencode": "agreed",
	}})

	if err := overrideSecretary(st, "gemini"); err != nil {
		t.Fatalf("override: %v", err)
	}
	if st.Secretary != "gemini" {
		t.Fatalf("secretary = %q", st.Secretary)
	}
	syn, err := converge(context.Background(), st, scriptRunner{
		replies: map[string]string{"gemini": "DECISIONS:\n- ship the homebrew tap first\nSUMMARY:\nClear."},
	})
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(syn.Decisions) != 1 {
		t.Fatalf("decisions = %v", syn.Decisions)
	}
	path, err := fileMinutes(st)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "ship the homebrew tap first") {
		t.Fatalf("recovered synthesis not in minutes:\n%s", md)
	}
	if strings.Contains(string(md), "UNKNOWN") {
		t.Fatal("the unknown notice survived a successful recovery pass")
	}
}

// The replacement secretary is still bound by the separation of powers.
func TestOverrideSecretaryRefusesAParticipant(t *testing.T) {
	st := newTestSession(t)
	err := overrideSecretary(st, "codex") // already a participant
	if err == nil {
		t.Fatal("expected a refusal: the secretary must not have a stake in the record")
	}
	if st.Secretary != "claude" {
		t.Fatalf("a refused override must not mutate the session; secretary = %q", st.Secretary)
	}
}
