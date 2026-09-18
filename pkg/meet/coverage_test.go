package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/capability"
)

// A board is a room full of conversation and no turns. Coverage must say so —
// the room-9 defect was coverage() ignoring message events entirely, so the
// minutes reported every seat empty while the transcript was full.
func TestCoverageCountsBoardMessages(t *testing.T) {
	st := newTestSession(t)
	events := []Event{
		{Kind: "message", Speaker: "codex", Text: "shipping the cursor fix", Chars: 23},
		{Kind: "message", Speaker: "codex", Text: "done", Chars: 4},
		{Kind: "turn", Speaker: "opencode", Text: "reviewed.", Status: statusOK, Chars: 9},
		{Kind: "message", Speaker: "stranger", Text: "not on the roster"},
	}
	rows := coverage(st, events)
	byName := map[string]Coverage{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	c := byName["codex"]
	if c.Messages != 2 || c.Turns != 0 {
		t.Errorf("codex = %+v; want 2 messages and 0 turns", c)
	}
	if c.OK != 0 {
		t.Errorf("codex OK = %d; a board post must never count as an OK turn — "+
			"OK feeds Verdict.decide()'s coverage ratio", c.OK)
	}
	if !c.Contributed() {
		t.Errorf("codex posted twice and still reads as an empty seat: %+v", c)
	}
	if c.Chars != 27 {
		t.Errorf("codex chars = %d, want 27", c.Chars)
	}
	if c.Last != "" {
		t.Errorf("codex last = %q; a message carries no turn status", c.Last)
	}
	if o := byName["opencode"]; o.Turns != 1 || o.OK != 1 || o.Messages != 0 {
		t.Errorf("opencode = %+v; want 1 ok turn and no messages", o)
	}
	if _, ok := byName["stranger"]; ok {
		t.Error("a non-roster speaker gained a coverage row")
	}
}

// The "never took a turn — the seat was empty" note may only fire for a seat
// that neither spoke nor posted.
func TestCoverageTableBoardSeatIsNotEmpty(t *testing.T) {
	st := newTestSession(t)
	events := []Event{{Kind: "message", Speaker: "codex", Text: "hello", Chars: 5}}
	var b strings.Builder
	writeCoverageTable(&b, coverage(st, events))
	out := b.String()
	if strings.Contains(out, "**codex** never took a turn") {
		t.Errorf("a seat that posted is not empty:\n%s", out)
	}
	if !strings.Contains(out, "**opencode** never took a turn") {
		t.Errorf("a seat that neither spoke nor posted must still read as empty:\n%s", out)
	}
}

func TestContributionsIncludeBoardMessages(t *testing.T) {
	st := newTestSession(t)
	events := []Event{
		{Round: 1, Kind: "message", Speaker: "codex", Text: "the cache is write-through", Chars: 26},
		{Round: 1, Kind: "turn", Speaker: "opencode", Text: "agreed", Status: statusOK},
	}
	var b strings.Builder
	writeContributions(&b, st, events, "")
	out := b.String()
	if !strings.Contains(out, "the cache is write-through") {
		t.Errorf("board message missing from contributions:\n%s", out)
	}
	if !strings.Contains(out, "codex → room") {
		t.Errorf("a broadcast post should read codex → room:\n%s", out)
	}
	var f strings.Builder
	writeContributions(&f, st, events, "codex")
	if got := f.String(); !strings.Contains(got, "write-through") || strings.Contains(got, "agreed") {
		t.Errorf("participant filter over messages is wrong:\n%s", got)
	}
}

func TestWriteEventRendersBoardMessage(t *testing.T) {
	e := Event{Round: 2, Kind: "message", Speaker: "codex", Text: "review posted"}
	var b strings.Builder
	writeEvent(&b, e, false)
	out := b.String()
	if want := seatLabel("codex") + " → room"; !strings.Contains(out, want) {
		t.Errorf("writeEvent = %q; want header containing %q", out, want)
	}
	if !strings.Contains(out, "review posted") {
		t.Errorf("writeEvent dropped the message body:\n%s", out)
	}
}

// A post with no recipient on the wire addresses the whole room.
func TestMessageToDefaultsToBroadcast(t *testing.T) {
	if got := messageTo(Event{Kind: "message", Speaker: "codex", Text: "hi"}); got != "" {
		t.Errorf("messageTo = %q, want empty for a broadcast", got)
	}
}

// A board records NOTHING into the capability matrix. A post proves the agent
// can type, not that its tool is operable, and operability feeds the routing
// table — counting chat traffic there would corrupt routing. Pinned so nobody
// "fixes" recordOperability to include message events.
func TestBoardMessagesDoNotRecordOperability(t *testing.T) {
	st := newTestSession(t) // isolates BASHY_CAPABILITY_DIR

	// Load() reconciles rows against the agent catalog, so only a real catalog
	// tool can be observed — pick one from the seeded priors.
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	var tool string
	for id := range m.Agents {
		tool = capability.ToolOf(id)
		break
	}
	if tool == "" {
		t.Fatal("seeded capability matrix has no agents to observe")
	}
	st.Participants = []string{tool}

	// RecordOperability fans out to every matrix row of the tool, so measure
	// the tool's total sample count.
	samples := func() int {
		m, err := capability.Load()
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for id, caps := range m.Agents {
			if capability.ToolOf(id) == tool {
				total += caps[capability.CapOperability].Samples
			}
		}
		return total
	}
	before := samples()
	recordOperability(st, []Event{
		{Kind: "message", Speaker: tool, Text: "posting away", Status: statusOK},
	})
	if got := samples(); got != before {
		t.Fatalf("a board message changed the capability matrix: %d -> %d samples", before, got)
	}
	// Positive control: the same speaker's real turn IS recorded, so this test
	// cannot pass vacuously against an empty or misdirected store.
	recordOperability(st, []Event{{Kind: "turn", Speaker: tool, Text: "a real turn", Status: statusOK}})
	if got := samples(); got <= before {
		t.Fatalf("control failed: a turn must add samples, got %d -> %d", before, got)
	}
}
