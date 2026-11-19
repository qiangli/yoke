package meet

import (
	"strings"
	"testing"
	"time"
)

// The S2 brief renders the room for one reader: new-for-you changes plus the
// standing state, permission-filtered, authority-labeled and bounded.

func briefRoom(t *testing.T, events []Event) *State {
	t.Helper()
	st := newTestSession(t) // human qiangli, participants codex/opencode
	base := fixedNow()
	for i, e := range events {
		e.TS = base.Add(time.Duration(i+1) * time.Second)
		if e.Role == "" && (e.Kind == "decision" || e.Kind == "action") {
			e.Role = string(RoleHuman)
		}
		if err := AppendEvent(st.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func briefFor(t *testing.T, st *State, reader string) *BriefResult {
	t.Helper()
	res, err := Brief(st.ID, BriefOptions{Reader: reader})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBriefActionIDMatchesS1Reference(t *testing.T) {
	// Independent vector: sha256("test-room" + NUL + "codex: check the gate")
	// computed outside this codebase; the brief must name the same candidate
	// S1 adopts, or the two stories diverge.
	if got := briefActionID("test-room", "codex: check the gate"); got != "act-e6188f69b3" {
		t.Fatalf("briefActionID = %q, want act-e6188f69b3 (S1 adopt.go scheme)", got)
	}
	if got := briefActionID("test-room", "  CODEX:  check   the GATE "); got != "act-e6188f69b3" {
		t.Fatalf("briefActionID must normalize case and whitespace like S1, got %q", got)
	}
}

func TestBriefFiltersDirectedMail(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "ship the brief"},
		{Speaker: "qiangli", Kind: "action", Text: "codex: check the gate"},
		{Speaker: "qiangli", Kind: "message", To: "opencode", Text: "your lane is quiet"},
		{Speaker: "qiangli", Kind: "message", To: "codex", Text: "your lane is next"},
		{Speaker: "codex", Kind: "turn", Text: "my own post"},
	})
	res := briefFor(t, st, "codex")

	for _, sec := range []BriefSection{res.Decisions, res.Proposals, res.Owned, res.Changes} {
		for _, it := range sec.Items {
			if it.Text == "your lane is quiet" {
				t.Fatalf("mail addressed to opencode leaked into codex brief: %+v", it)
			}
		}
	}
	found := false
	for _, it := range res.Changes.Items {
		if it.Text == "your lane is next" {
			found = true
			if !it.Directed {
				t.Fatalf("mail to codex must carry the directed flag: %+v", it)
			}
		}
		if it.Text == "my own post" {
			t.Fatalf("reader's own post is not news: %+v", it)
		}
	}
	if !found {
		t.Fatalf("mail addressed to codex missing from changes: %+v", res.Changes)
	}
}

func TestBriefSinceCursorWindow(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "old decision"},
	})
	if err := MarkSeen(st.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	at := fixedNow().Add(99 * time.Second)
	if err := AppendEvent(st.ID, Event{Speaker: "qiangli", Kind: "decision", Text: "new decision", TS: at}); err != nil {
		t.Fatal(err)
	}
	res := briefFor(t, st, "codex")
	if res.Seen != 1 || res.Through != 2 {
		t.Fatalf("seen/through = %d/%d, want 1/2", res.Seen, res.Through)
	}
	if len(res.Changes.Items) != 1 || res.Changes.Items[0].Text != "new decision" {
		t.Fatalf("changes = %+v, want only the new decision", res.Changes)
	}
	if len(res.Decisions.Items) != 2 {
		t.Fatalf("standing decisions must keep history, got %+v", res.Decisions)
	}
	if !res.Decisions.Items[1].New {
		t.Fatal("the new decision must carry the new flag in standing state")
	}
}

func TestBriefLeavesCursorUnlessMarked(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "d1"},
	})
	before := SeenSeq(st.ID, "codex")
	if _, err := Brief(st.ID, BriefOptions{Reader: "codex"}); err != nil {
		t.Fatal(err)
	}
	if got := SeenSeq(st.ID, "codex"); got != before {
		t.Fatalf("brief advanced the cursor %d -> %d without --mark-seen", before, got)
	}
	if _, err := Brief(st.ID, BriefOptions{Reader: "codex", MarkSeen: true}); err != nil {
		t.Fatal(err)
	}
	if got := SeenSeq(st.ID, "codex"); got != 1 {
		t.Fatalf("mark-seen cursor = %d, want 1", got)
	}
}

func TestBriefRetractionWithdrawsAction(t *testing.T) {
	st := newTestSession(t)
	base := fixedNow()
	actionTS := base.Add(time.Second)
	if err := AppendEvent(st.ID, Event{Speaker: "qiangli", Kind: "action", Role: string(RoleHuman), Text: "codex: check the gate", TS: actionTS}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(st.ID, Event{
		Speaker: "qiangli", Kind: "retraction", Role: string(RoleHuman),
		To: "", Text: "disregard the gate check",
		Retracts: actionTS.Format(time.RFC3339Nano), TS: base.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	res := briefFor(t, st, "opencode")
	if len(res.Proposals.Items) != 0 {
		t.Fatalf("retracted action must leave proposals, got %+v", res.Proposals)
	}
	if len(res.Owned.Items) != 0 {
		t.Fatalf("retracted action must leave owned work, got %+v", res.Owned)
	}
	if len(res.Objections.Items) != 1 || res.Objections.Items[0].Kind != "retraction" {
		t.Fatalf("retraction must stay visible as an objection, got %+v", res.Objections)
	}
}

func TestBriefPreservesAuthorityDistinction(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
		{Speaker: "qiangli", Kind: "action", Text: "codex: check the gate"},
	})
	syn := &Synthesis{
		By: "claude", At: fixedNow().Add(time.Hour), Mode: "infer",
		Decisions:     []Decision{{Text: "we ship friday", Inferred: true}},
		Actions:       []string{"opencode: sweep the logs"},
		OpenQuestions: []string{"who owns the release?"},
		Risks:         []string{"gate flakiness"},
	}
	if err := syn.save(st.ID); err != nil {
		t.Fatal(err)
	}
	res := briefFor(t, st, "opencode")
	if len(res.Decisions.Items) != 1 || res.Decisions.Items[0].Authority != "human" {
		t.Fatalf("human-marked decision must be authoritative: %+v", res.Decisions)
	}
	var agreed, proposed int
	for _, it := range res.Proposals.Items {
		switch {
		case it.Authority == "human" && it.Source == "agreed":
			agreed++
			if it.ID == "" {
				t.Fatalf("agreed action needs the S1 decision reference: %+v", it)
			}
		case it.Authority == "secretary-synthesis" && it.Source == "proposed":
			proposed++
		default:
			t.Fatalf("unexpected proposal authority: %+v", it)
		}
	}
	if agreed != 1 || proposed != 2 { // one synthesis action + one synthesis decision
		t.Fatalf("proposals = %+v, want 1 agreed + 2 proposed", res.Proposals)
	}
	if len(res.Questions) != 1 || res.Questions[0].Authority != "secretary-synthesis" {
		t.Fatalf("synthesis open questions must stay labeled proposals: %+v", res.Questions)
	}
	if len(res.Objections.Items) != 1 || res.Objections.Items[0].Authority != "secretary-synthesis" {
		t.Fatalf("synthesis risks must stay labeled objections: %+v", res.Objections)
	}
}

func TestBriefKeepsDissentBesideDecision(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "poll", Question: "ship friday?", Choices: []string{"yes", "no"}},
		{Speaker: "codex", Kind: "vote", Choice: "no", Choices: []string{"yes", "no"}, Text: "no: gate is red"},
		{Speaker: "opencode", Kind: "vote", Choice: "yes", Choices: []string{"yes", "no"}, Text: "yes"},
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
	})
	res := briefFor(t, st, "codex")
	if len(res.Decisions.Items) != 1 {
		t.Fatalf("decisions = %+v", res.Decisions)
	}
	if len(res.Objections.Items) != 1 || !strings.Contains(res.Objections.Items[0].Text, "gate is red") {
		t.Fatalf("the no-vote must survive beside the decision: %+v", res.Objections)
	}
	if len(res.Questions) != 1 || res.Questions[0].Votes != 2 {
		t.Fatalf("poll must carry both votes: %+v", res.Questions)
	}
	if got := res.Questions[0].Tally["no"]; got != 1 {
		t.Fatalf("poll tally = %+v, want no=1", res.Questions[0].Tally)
	}
}

func TestBriefBoundsLists(t *testing.T) {
	var events []Event
	for i := 0; i < BriefLimit+5; i++ {
		events = append(events, Event{Speaker: "qiangli", Kind: "action", Text: "task"})
	}
	// Distinct texts so nothing dedupes: each is its own candidate.
	for i := range events {
		events[i].Text = "task number " + strings.Repeat("x", i+1)
	}
	st := briefRoom(t, events)
	res, err := Brief(st.ID, BriefOptions{Reader: "codex", Limit: BriefLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Proposals.Items) != BriefLimit || res.Proposals.Hidden != 5 {
		t.Fatalf("proposals bounded = %d+%d, want %d+5", len(res.Proposals.Items), res.Proposals.Hidden, BriefLimit)
	}
	if len(res.Changes.Items) != BriefLimit || res.Changes.Hidden != 5 {
		t.Fatalf("changes bounded = %d+%d, want %d+5", len(res.Changes.Items), res.Changes.Hidden, BriefLimit)
	}
}

func TestBriefOwnedFlagsMine(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "action", Text: "codex: check the gate"},
		{Speaker: "qiangli", Kind: "action", Text: "opencode: sweep the logs"},
		{Speaker: "qiangli", Kind: "action", Text: "check the docs"}, // no owner prefix
	})
	res := briefFor(t, st, "codex")
	if len(res.Owned.Items) != 2 {
		t.Fatalf("owned = %+v, want the two prefixed actions", res.Owned)
	}
	mine := 0
	for _, it := range res.Owned.Items {
		if it.Owner == "" || it.ID == "" {
			t.Fatalf("owned work needs an owner and an S1 reference: %+v", it)
		}
		if it.Mine {
			mine++
			if it.Owner != "codex" {
				t.Fatalf("mine flag on another owner's work: %+v", it)
			}
		}
	}
	if mine != 1 {
		t.Fatalf("exactly one item is mine, got %+v", res.Owned)
	}
}

func TestBriefRequiresAReader(t *testing.T) {
	st := briefRoom(t, []Event{{Speaker: "qiangli", Kind: "decision", Text: "d"}})
	if _, err := Brief(st.ID, BriefOptions{}); err == nil {
		t.Fatal("a brief without a reader must fail: filtering and the cursor belong to somebody")
	}
	if _, err := Brief("no-such-room", BriefOptions{Reader: "codex"}); err == nil {
		t.Fatal("a brief for a missing room must fail")
	}
}

func TestBriefCommandRendersAndEmitsJSON(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
		{Speaker: "qiangli", Kind: "action", Text: "codex: check the gate"},
	})
	cmd := newBriefCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{st.ID, "--as", "codex"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"confirmed decisions", "proposals", "owned work", "pending questions", "act-"} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief text missing %q:\n%s", want, text)
		}
	}
	cmd = newBriefCmd()
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{st.ID, "--as", "codex", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decisions"`, `"proposals"`, `"owned"`, `"questions"`, `"authority": "human"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("brief JSON missing %s:\n%s", want, out.String())
		}
	}
}
