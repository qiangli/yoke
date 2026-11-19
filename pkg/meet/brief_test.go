package meet

import (
	"encoding/json"
	"net/http"
	"net/url"
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
	// Caps keep the NEWEST in stable transcript order: the five oldest hide,
	// the newest decision is present, and the kept tail stays chronological.
	if len(res.Proposals.Items) != BriefLimit || res.Proposals.Hidden != 5 {
		t.Fatalf("proposals bounded = %d+%d, want %d+5", len(res.Proposals.Items), res.Proposals.Hidden, BriefLimit)
	}
	first, last := res.Proposals.Items[0], res.Proposals.Items[len(res.Proposals.Items)-1]
	if want := "task number " + strings.Repeat("x", 6); first.Text != want {
		t.Fatalf("capped proposals must start at the 6th action, got %q want %q", first.Text, want)
	}
	if want := "task number " + strings.Repeat("x", BriefLimit+5); last.Text != want {
		t.Fatalf("capped proposals must end at the newest action, got %q want %q", last.Text, want)
	}
	for i := 1; i < len(res.Proposals.Items); i++ {
		if res.Proposals.Items[i].Seq <= res.Proposals.Items[i-1].Seq {
			t.Fatalf("capped proposals must keep transcript order: %+v", res.Proposals.Items)
		}
	}
	if len(res.Changes.Items) != BriefLimit || res.Changes.Hidden != 5 {
		t.Fatalf("changes bounded = %d+%d, want %d+5", len(res.Changes.Items), res.Changes.Hidden, BriefLimit)
	}
	if got, want := res.Changes.Items[len(res.Changes.Items)-1].Text, "task number "+strings.Repeat("x", BriefLimit+5); got != want {
		t.Fatalf("capped changes must end at the newest event, got %q want %q", got, want)
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

// The secretary reads the whole transcript, directed mail included, so a
// global synthesis written where the transcript holds mail this reader may
// not see cannot be proven clean for them — neither verbatim quotation nor
// paraphrase. The synthesis is withheld from that reader entirely, the
// omission is flagged, and the brief shows only events they may see. The
// addressee's own brief is unaffected: their mail is theirs.
func TestBriefSynthesisDoesNotLaunderDirectedMail(t *testing.T) {
	const secret = "the vault combination is midnight-blue-seven"
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
		{Speaker: "qiangli", Kind: "message", To: "opencode", Text: "for you only: " + secret},
	})
	syn := &Synthesis{
		By: "claude", At: fixedNow().Add(time.Hour), Mode: "infer",
		Decisions:     []Decision{{Text: "we ship friday, since " + secret, Inferred: true}},
		Actions:       []string{"opencode: rotate " + secret},
		OpenQuestions: []string{"who else knows the combination?", "who else knows " + secret + "?"},
		Risks:         []string{"exposure of " + secret, "gate flakiness may slip the date"},
	}
	if err := syn.save(st.ID); err != nil {
		t.Fatal(err)
	}

	res, err := Brief(st.ID, BriefOptions{Reader: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "midnight-blue-seven") {
		t.Fatalf("another reader's brief must not quote mail directed elsewhere:\n%s", raw)
	}
	if !res.SynthesisOmitted {
		t.Fatalf("withholding the synthesis must be flagged, not silent: %+v", res)
	}
	for _, sec := range []BriefSection{res.Proposals, res.Objections, res.Owned} {
		for _, it := range sec.Items {
			if it.Speaker == "secretary" {
				t.Fatalf("withheld synthesis must leave every section: %+v", it)
			}
		}
	}
	for _, q := range res.Questions {
		// "who else knows the combination?" paraphrases the secret mail
		// without quoting it: paraphrase is withheld with the rest.
		if q.Speaker == "secretary" {
			t.Fatalf("paraphrase of private mail is withheld with the synthesis: %+v", q)
		}
	}
	// The human-marked decision is room state and stays.
	if len(res.Decisions.Items) != 1 || res.Decisions.Items[0].Text != "we ship friday" {
		t.Fatalf("public decision must survive synthesis withholding: %+v", res.Decisions)
	}

	// The addressee's brief is unfiltered: their own mail, and the synthesis
	// quoting it, are theirs to see.
	got, err := Brief(st.ID, BriefOptions{Reader: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SynthesisOmitted {
		t.Fatalf("the addressee's synthesis must not be withheld: %+v", got)
	}
	oraw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oraw), "midnight-blue-seven") {
		t.Fatalf("the addressee must still see their own mail and its synthesis:\n%s", oraw)
	}
}

// The web route binds the reader to the verified caller: a vouched request
// naming another participant is refused before any content or cursor moves,
// while the loopback machine-owner path keeps the CLI's --as local model.
func TestBriefWebBindsReaderToVouchedCaller(t *testing.T) {
	const secret = "codex lane holds the midnight vault token"
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "public decision"},
		{Speaker: "qiangli", Kind: "message", To: "codex", Text: secret},
	})
	srv := serveTest(t)
	const attacker, victim = "attacker@example.com", "codex"
	ref := "/api/rooms/" + url.PathEscape(st.ID) + "/brief"

	before := SeenSeq(st.ID, victim)

	for _, q := range []string{"?as=" + victim, "?as=" + victim + "&mark_seen=1"} {
		resp, body := doVouchedJSON(t, srv, attacker, "GET", ref+q, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-principal brief %s = %d, want 403 (body %s)", q, resp.StatusCode, body)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("a refused brief must carry no victim-directed mail: %s", body)
		}
	}
	// The ack route binds the same way: a vouched POST for another
	// participant is refused and moves no cursor.
	for _, q := range []string{"?as=" + victim + "&mark_seen=1", "?as=" + victim + "&mark_seen=1&ack_all=1"} {
		resp, body := doVouchedJSON(t, srv, attacker, "POST", ref+q, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-principal brief ack %s = %d, want 403 (body %s)", q, resp.StatusCode, body)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("a refused brief ack must carry no victim-directed mail: %s", body)
		}
	}
	if got := SeenSeq(st.ID, victim); got != before {
		t.Fatalf("a refused brief moved the victim cursor %d -> %d", before, got)
	}

	resp, body := doVouchedJSON(t, srv, attacker, "GET", ref, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a caller's own brief = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var own BriefResult
	if err := json.Unmarshal(body, &own); err != nil {
		t.Fatalf("decode own brief: %v (%s)", err, body)
	}
	if strings.EqualFold(own.Reader, victim) {
		t.Fatalf("a vouched brief reader = %q, want the caller, not the victim", own.Reader)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("a caller's own brief must not carry victim-directed mail: %s", body)
	}

	// Naming yourself stays allowed, and the ack route moves your cursor.
	resp, body = doVouchedJSON(t, srv, attacker, "POST", ref+"?as="+url.QueryEscape(own.Reader)+"&mark_seen=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a self-named brief ack = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if got := SeenSeq(st.ID, readerName(own.Reader)); got == 0 {
		t.Fatal("a self-named POST ack must advance the caller's own cursor")
	}

	// Loopback is the machine owner at their keyboard: ?as= names a seat,
	// exactly like the CLI's --as, and the seat's mail is there.
	resp, body = doJSON(t, srv, "GET", ref+"?as="+victim, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a loopback ?as= brief = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var local BriefResult
	if err := json.Unmarshal(body, &local); err != nil {
		t.Fatalf("decode loopback brief: %v (%s)", err, body)
	}
	if !strings.EqualFold(local.Reader, victim) || !strings.Contains(string(body), secret) {
		t.Fatalf("a loopback ?as= brief must brief the named seat with its mail: reader %q", local.Reader)
	}

	// GET never moves a cursor, even when asked: prefetch and retry cannot
	// consume the inbox the brief points at. This room is created last:
	// briefRoom re-points the meet dir, and rooms share a deterministic test
	// ID, so nothing above may read the earlier room after this point.
	markRoom := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "public decision two"},
	})
	markRef := "/api/rooms/" + url.PathEscape(markRoom.ID) + "/brief"
	markBefore := SeenSeq(markRoom.ID, victim)
	resp, body = doJSON(t, srv, "GET", markRef+"?as="+victim+"&mark_seen=1&ack_all=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a loopback GET brief = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if got := SeenSeq(markRoom.ID, victim); got != markBefore {
		t.Fatalf("GET with ?mark_seen=1 moved the cursor %d -> %d", markBefore, got)
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
	for _, want := range []string{"confirmed decisions", "proposals", "owned work", "open questions", "act-"} {
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

// A decision or action is authoritative only when a roster member marked it:
// the same kind from outside the roster is a proposal in a participant's
// voice, never a confirmation, and it founds no owned work.
func TestBriefAgentDecisionIsNotAuthority(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "intruder", Kind: "decision", Text: "we ship at dawn"},
		{Speaker: "intruder", Kind: "action", Text: "codex: open the gate"},
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
	})
	res := briefFor(t, st, "codex")
	if len(res.Decisions.Items) != 1 || res.Decisions.Items[0].Text != "we ship friday" {
		t.Fatalf("only the member's decision confirms: %+v", res.Decisions)
	}
	if len(res.Decisions.Items) != 1 || res.Decisions.Items[0].Authority != "human" {
		t.Fatalf("member decision must carry human authority: %+v", res.Decisions)
	}
	var voice, withID int
	for _, it := range res.Proposals.Items {
		if it.Authority == "participant" && it.Source == "proposed" {
			voice++
		}
		if it.ID != "" {
			withID++
		}
	}
	if voice != 2 {
		t.Fatalf("both outsider items must be voice proposals: %+v", res.Proposals)
	}
	if withID != 0 {
		t.Fatalf("outsider actions found no agreement, so no S1 reference: %+v", res.Proposals)
	}
	if len(res.Owned.Items) != 0 {
		t.Fatalf("outsider actions found no owned work: %+v", res.Owned)
	}
}

// Pending means evidenced-unresolved: a decided poll, an answered ask, a
// retracted question and a retracted vote are history, and retracted votes
// tally nowhere. Live disagreement on an undecided ballot stays.
func TestBriefShowsOnlyUnresolvedQuestions(t *testing.T) {
	st := newTestSession(t)
	base := fixedNow()
	append := func(e Event, d time.Duration) {
		t.Helper()
		e.TS = base.Add(d)
		if err := AppendEvent(st.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	// Decided poll: yes outnumbers no with no spoilage — history.
	append(Event{Speaker: "qiangli", Kind: "poll", Question: "ship?", Choices: []string{"yes", "no"}}, time.Second)
	append(Event{Speaker: "codex", Kind: "vote", Choice: "yes", Choices: []string{"yes", "no"}, Text: "yes"}, 2*time.Second)
	append(Event{Speaker: "opencode", Kind: "vote", Choice: "yes", Choices: []string{"yes", "no"}, Text: "yes"}, 3*time.Second)
	append(Event{Speaker: "qiangli", Kind: "vote", Choice: "no", Choices: []string{"yes", "no"}, Text: "no: gate"}, 4*time.Second)
	// Retracted question: withdrawn, not pending.
	qTS := base.Add(5 * time.Second)
	append(Event{Speaker: "qiangli", Kind: "question", Question: "old worry?", Text: "old worry?"}, 5*time.Second)
	append(Event{Speaker: "qiangli", Kind: "retraction", Text: "never mind", Retracts: qTS.Format(time.RFC3339Nano)}, 6*time.Second)
	// Retracted vote on the live ballot below: tallies nowhere, dissents
	// nowhere. The ballot stays tied 1-1 without it — pending.
	append(Event{Speaker: "qiangli", Kind: "poll", Question: "slip?", Choices: []string{"yes", "no"}}, 7*time.Second)
	vTS := base.Add(8 * time.Second)
	append(Event{Speaker: "codex", Kind: "vote", Choice: "no", Choices: []string{"yes", "no"}, Text: "no: red"}, 8*time.Second)
	append(Event{Speaker: "codex", Kind: "retraction", Text: "take that back", Retracts: vTS.Format(time.RFC3339Nano)}, 9*time.Second)
	append(Event{Speaker: "opencode", Kind: "vote", Choice: "yes", Choices: []string{"yes", "no"}, Text: "yes"}, 10*time.Second)
	append(Event{Speaker: "qiangli", Kind: "vote", Choice: "no", Choices: []string{"yes", "no"}, Text: "no: amber"}, 11*time.Second)
	// Answered ask: a later successful turn names the question link.
	append(Event{Speaker: "qiangli", Kind: "ask", Question: "any blockers?", Text: "any blockers?"}, 12*time.Second)
	append(Event{Speaker: "codex", Kind: "turn", Question: "any blockers?", Status: statusOK, Text: "none here"}, 13*time.Second)
	// Unanswered ask: honestly pending.
	append(Event{Speaker: "qiangli", Kind: "ask", Question: "who owns docs?", Text: "who owns docs?"}, 14*time.Second)

	res := briefFor(t, st, "codex")
	for _, q := range res.Questions {
		switch q.Question {
		case "ship?", "old worry?", "any blockers?":
			t.Fatalf("resolved question must leave pending: %+v", q)
		}
		if q.Question == "slip?" {
			// Tied 1-1 on live votes only: had the retracted no-vote
			// counted, the ballot would read no=2 and be decided.
			if got := q.Tally["no"]; got != 1 {
				t.Fatalf("retracted vote must tally nowhere, no=%d in %+v", got, q.Tally)
			}
			if q.Votes != 2 {
				t.Fatalf("retracted vote must not count, votes=%d", q.Votes)
			}
		}
	}
	if len(res.Questions) != 2 {
		t.Fatalf("only the tied poll and the unanswered ask stay pending: %+v", res.Questions)
	}
	for _, it := range res.Objections.Items {
		if it.Text == "no: red" {
			t.Fatalf("a retracted vote is withdrawn dissent: %+v", it)
		}
	}
	found := false
	for _, it := range res.Objections.Items {
		if it.Text == "no: gate" {
			found = true // disagreement on the decided poll is preserved
		}
	}
	if !found {
		t.Fatalf("dissent on a decided poll must survive beside it: %+v", res.Objections)
	}
}

// When caps hide something the reader has not seen, --mark-seen refuses
// rather than silently consume the hidden tail; --ack-all acknowledges it
// explicitly.
func TestBriefMarkSeenRefusesHiddenTail(t *testing.T) {
	var events []Event
	for i := 0; i < BriefLimit+5; i++ {
		events = append(events, Event{Speaker: "qiangli", Kind: "action", Text: "task"})
	}
	for i := range events {
		events[i].Text = "task number " + strings.Repeat("x", i+1)
	}
	st := briefRoom(t, events)
	if _, err := Brief(st.ID, BriefOptions{Reader: "codex", MarkSeen: true}); err == nil {
		t.Fatal("mark-seen over a hidden unseen tail must refuse without --ack-all")
	}
	if got := SeenSeq(st.ID, "codex"); got != 0 {
		t.Fatalf("a refused mark-seen must not move the cursor, got %d", got)
	}
	res, err := Brief(st.ID, BriefOptions{Reader: "codex", MarkSeen: true, AckAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := SeenSeq(st.ID, "codex"); got != res.Through {
		t.Fatalf("ack-all cursor = %d, want through %d", got, res.Through)
	}
	again, err := Brief(st.ID, BriefOptions{Reader: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes.Items) != 0 {
		t.Fatalf("after ack-all nothing is new: %+v", again.Changes)
	}
}

// A huge turn cannot grow the brief without bound: item text is capped with
// a marker, and the whole brief keeps a byte budget even at inflated limits.
func TestBriefBoundsHugeTurns(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: strings.Repeat("dump ", 200000)},
	})
	res := briefFor(t, st, "codex")
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > BriefTotalBytes+4096 {
		t.Fatalf("one huge turn grew the brief to %d bytes", len(raw))
	}
	if !strings.Contains(res.Decisions.Items[0].Text, "[truncated]") {
		t.Fatalf("capped item text must say so: %.80q...", res.Decisions.Items[0].Text)
	}
	if len(res.Decisions.Items[0].Text) > BriefItemTextBytes {
		t.Fatalf("item text %d bytes exceeds the per-item cap", len(res.Decisions.Items[0].Text))
	}

	// Inflated limits still fit the total budget: oldest yields, newest stays.
	var many []Event
	for i := 0; i < 300; i++ {
		many = append(many, Event{Speaker: "qiangli", Kind: "decision", Text: "d" + strings.Repeat("y", 1000)})
	}
	st2 := briefRoom(t, many)
	big, err := Brief(st2.ID, BriefOptions{Reader: "codex", Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if big.Decisions.Hidden == 0 {
		t.Fatalf("the total budget must hide oldest items at inflated limits: %+v", big.Decisions)
	}
	braw, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(braw) > BriefTotalBytes+65536 {
		t.Fatalf("inflated limits grew the brief to %d bytes", len(braw))
	}
	last := big.Decisions.Items[len(big.Decisions.Items)-1]
	if last.Seq != 300 {
		t.Fatalf("budget cuts must keep the newest decision, last seq = %d", last.Seq)
	}
}

// A terse secret counts exactly like a long one: directed mail of any length
// withholds the secretary synthesis from every other reader, because presence
// — not quotability — is what a whole-transcript summary cannot prove clean.
func TestBriefShortDirectedMailWithholdsSynthesis(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
		{Speaker: "qiangli", Kind: "message", To: "opencode", Text: "PIN 1234"},
	})
	syn := &Synthesis{
		By: "claude", At: fixedNow().Add(time.Hour), Mode: "infer",
		Decisions: []Decision{{Text: "we ship friday", Inferred: true}},
		Risks:     []string{"gate flakiness may slip the date"},
	}
	if err := syn.save(st.ID); err != nil {
		t.Fatal(err)
	}
	res, err := Brief(st.ID, BriefOptions{Reader: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "1234") {
		t.Fatalf("another reader's brief must not carry terse directed mail:\n%s", raw)
	}
	if !res.SynthesisOmitted {
		t.Fatal("withholding the synthesis must be flagged, not silent")
	}
	for _, sec := range []BriefSection{res.Proposals, res.Objections, res.Owned} {
		for _, it := range sec.Items {
			if it.Speaker == "secretary" {
				t.Fatalf("withheld synthesis must leave every section: %+v", it)
			}
		}
	}
	// The addressee's brief is unaffected: their own mail is theirs.
	got, err := Brief(st.ID, BriefOptions{Reader: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SynthesisOmitted {
		t.Fatalf("the addressee's synthesis must not be withheld: %+v", got)
	}
}

// A decision, action or confirm from an agent seat is a participant's voice,
// never a confirmation — even when the seat is rostered. Only one of the
// room's people confirms, and only a confirmation founds agreement and owned
// work. The changes window carries the same labels as the sections.
func TestBriefAgentSeatIsNotAuthority(t *testing.T) {
	st := briefRoom(t, []Event{
		{Speaker: "codex", Kind: "decision", Text: "we ship at dawn"},
		{Speaker: "codex", Kind: "action", Text: "codex: open the gate"},
		{Speaker: "codex", Kind: "confirm", Text: "gate is green"},
		{Speaker: "qiangli", Kind: "decision", Text: "we ship friday"},
		{Speaker: "qiangli", Kind: "action", Text: "opencode: sweep the logs"},
	})
	res := briefFor(t, st, "opencode")
	if len(res.Decisions.Items) != 1 || res.Decisions.Items[0].Text != "we ship friday" {
		t.Fatalf("only the human decision confirms: %+v", res.Decisions)
	}
	if res.Decisions.Items[0].Authority != "human" {
		t.Fatalf("the human decision must carry human authority: %+v", res.Decisions)
	}
	voice, agreed := 0, 0
	for _, it := range res.Proposals.Items {
		switch {
		case it.Authority == "participant" && it.Source == "proposed":
			voice++
			if it.ID != "" {
				t.Fatalf("a voice proposal founds no S1 reference: %+v", it)
			}
		case it.Authority == "human" && it.Source == "agreed":
			agreed++
		default:
			t.Fatalf("unexpected proposal authority: %+v", it)
		}
	}
	if voice != 3 {
		t.Fatalf("the agent seat's decision, action and confirm are voices: %+v", res.Proposals)
	}
	if agreed != 1 {
		t.Fatalf("the human action agrees once: %+v", res.Proposals)
	}
	if len(res.Owned.Items) != 1 || res.Owned.Items[0].Owner != "opencode" || !res.Owned.Items[0].Mine {
		t.Fatalf("owned work comes only from the agreed action: %+v", res.Owned)
	}
	for _, it := range res.Changes.Items {
		if it.Speaker == "codex" && it.Authority != "participant" {
			t.Fatalf("the window must not upgrade a seat's voice: %+v", it)
		}
		if it.Speaker == "qiangli" && (it.Kind == "decision" || it.Kind == "action") && it.Authority != "human" {
			t.Fatalf("the window must keep human authority: %+v", it)
		}
	}
}

// A huge ballot stays bounded: the question and every choice truncate with a
// marker, excess choices are counted, the full ballot stays one `meet read`
// away — and the votes still tally, because matching runs on the same
// bounded forms.
func TestBriefHugePollStaysBounded(t *testing.T) {
	choices := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		choices = append(choices, "option-"+strings.Repeat("xy", 1000)+"-"+strings.Repeat("z", i%7))
	}
	st := newTestSession(t)
	base := fixedNow()
	poll := Event{
		Speaker: "qiangli", Kind: "poll", Role: string(RoleChair),
		Question: strings.Repeat("ship? ", 60000),
		Choices:  choices, Text: "poll open",
		TS: base.Add(time.Second),
	}
	if err := AppendEvent(st.ID, poll); err != nil {
		t.Fatal(err)
	}
	for i, c := range []string{choices[3], choices[7]} {
		if err := AppendEvent(st.ID, Event{
			Speaker: "codex", Kind: "vote", Choice: c, Choices: choices,
			Text: "vote", TS: base.Add(time.Duration(2+i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	res := briefFor(t, st, "codex")
	if len(res.Questions) != 1 {
		t.Fatalf("the undecided huge poll stays pending: %+v", res.Questions)
	}
	q := res.Questions[0]
	if len(q.Question) > BriefItemTextBytes || !strings.Contains(q.Question, "[truncated]") {
		t.Fatalf("the huge question must truncate with a marker, got %d bytes", len(q.Question))
	}
	if len(q.Choices) != BriefPollChoiceLimit || q.ChoicesHidden != len(choices)-BriefPollChoiceLimit {
		t.Fatalf("ballot kept %d+%d, want %d+%d", len(q.Choices), q.ChoicesHidden, BriefPollChoiceLimit, len(choices)-BriefPollChoiceLimit)
	}
	for _, c := range q.Choices {
		if len(c) > BriefChoiceTextBytes || !strings.Contains(c, "[truncated]") {
			t.Fatalf("every huge choice must truncate with a marker, got %d bytes", len(c))
		}
	}
	if q.Votes != 2 {
		t.Fatalf("both votes must tally on the bounded ballot: %+v", q.Tally)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > BriefTotalBytes {
		t.Fatalf("one huge poll grew the brief to %d bytes", len(raw))
	}
}

// The byte budget hides like a cap: when it covers unread items, --mark-seen
// refuses without --ack-all instead of silently consuming the hidden tail.
func TestBriefBudgetHiddenTailRefusesMarkSeen(t *testing.T) {
	var events []Event
	for i := 0; i < 300; i++ {
		events = append(events, Event{Speaker: "qiangli", Kind: "decision", Text: "d" + strings.Repeat("y", 1000)})
	}
	st := briefRoom(t, events)
	res, err := Brief(st.ID, BriefOptions{Reader: "codex", Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	// No per-list cap hid anything at this limit: the budget did the hiding.
	if res.Decisions.Hidden == 0 && res.Changes.Hidden == 0 {
		t.Fatalf("the total budget must hide oldest items at inflated limits: %+v %+v", res.Decisions, res.Changes)
	}
	if _, err := Brief(st.ID, BriefOptions{Reader: "codex", Limit: 10000, MarkSeen: true}); err == nil {
		t.Fatal("mark-seen over a budget-hidden unseen tail must refuse without --ack-all")
	}
	if got := SeenSeq(st.ID, "codex"); got != 0 {
		t.Fatalf("a refused mark-seen must not move the cursor, got %d", got)
	}
	acked, err := Brief(st.ID, BriefOptions{Reader: "codex", Limit: 10000, MarkSeen: true, AckAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := SeenSeq(st.ID, "codex"); got != acked.Through {
		t.Fatalf("ack-all cursor = %d, want through %d", got, acked.Through)
	}
}
