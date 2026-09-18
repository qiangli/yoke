package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

// THE GAP THIS CLOSES. Before R0 there were no subscriptions; after R0 there
// were inboxes but nothing resolved into them, because no sidecar process
// existed on any host. A DM sat in the timeline and `pending` truthfully
// reported an empty buffer while the message existed.
func TestResolveFor_DeliversWithNoSidecarRunning(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{
		Type: room.EventNotify, To: "claude-opus5", Topic: "cert", Body: "gate is red",
	}); err != nil {
		t.Fatal(err)
	}

	// No Sidecar is constructed anywhere in this test — that is the point.
	n, err := ResolveFor("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("resolved %d, want 1", n)
	}
	items, err := ReadPending("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Body != "gate is red" {
		t.Fatalf("pending = %+v", items)
	}
	if items[0].Delivery != DeliveryQueued {
		t.Fatalf("delivery = %q, want queued — the agent is reading, so an interrupt would break the turn that came to collect it", items[0].Delivery)
	}
}

// Resolving twice must not deliver twice: the offset advances after the append.
func TestResolveFor_IsIdempotent(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "claude-opus5", Body: "once"}); err != nil {
		t.Fatal(err)
	}
	if n, err := ResolveFor("claude-opus5"); err != nil || n != 1 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}
	if n, err := ResolveFor("claude-opus5"); err != nil || n != 0 {
		t.Fatalf("second pass must resolve nothing: n=%d err=%v", n, err)
	}
}

// A message for somebody else must never land in this agent's buffer.
func TestResolveFor_OnlyItsOwnMail(t *testing.T) {
	busInTempHome(t)
	for _, who := range []string{"claude-opus5", "codex-gpt5.6-sol"} {
		if _, err := EnsureSubscription(who); err != nil {
			t.Fatal(err)
		}
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "codex-gpt5.6-sol", Body: "not yours"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := ResolveFor("claude-opus5"); n != 0 {
		t.Fatalf("resolved %d for the wrong subscriber", n)
	}
	if n, _ := ResolveFor("codex-gpt5.6-sol"); n != 1 {
		t.Fatalf("the addressee resolved %d, want 1", n)
	}
}

// A name outside the address book resolves nothing and is not an error —
// refusing to print an empty buffer for it would help nobody.
func TestResolveFor_UnknownSubscriberIsSilent(t *testing.T) {
	busInTempHome(t)
	n, err := ResolveFor("not-an-agent")
	if err != nil {
		t.Fatalf("an unknown subscriber must not error: %v", err)
	}
	if n != 0 {
		t.Fatalf("resolved %d, want 0", n)
	}
}

// A cold agent — one that was not running when the message was sent — gets it
// on the next read. That is the whole (c) half of the delivery model.
func TestResolveFor_ColdAgentGetsItLater(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("agy-gemini3.1"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first", "second"} {
		if err := room.Emit(room.Event{Type: room.EventNotify, To: "agy-gemini3.1", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := ResolveFor("agy-gemini3.1"); n != 2 {
		t.Fatalf("a cold agent must receive everything sent while it was away, got %d", n)
	}
}

// UNREAD ON LOGIN. The launch path cannot use TurnPreamble — that keys on the
// control socket, and at launch there is no socket yet, because it is created
// by the very call that needs the mail. An agent is known by NAME first.
func TestLaunchPreamble_DeliversToAColdAgentByName(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("codex-gpt5.6-sol"); err != nil {
		t.Fatal(err)
	}
	// Sent while the agent was not running, with no sidecar watching — the
	// common case on a real host.
	if err := room.Emit(room.Event{
		Type: room.EventNotify, To: "codex-gpt5.6-sol", Topic: "fleet", Body: "roster changed",
	}); err != nil {
		t.Fatal(err)
	}

	block := LaunchPreamble("codex-gpt5.6-sol")
	if block == "" {
		t.Fatal("a cold agent must receive its mail on the way in")
	}
	if !strings.Contains(block, "roster changed") {
		t.Fatalf("preamble is missing the message:\n%s", block)
	}
	// Rendered once: a second launch must not re-deliver what was already shown.
	if again := LaunchPreamble("codex-gpt5.6-sol"); again != "" {
		t.Fatalf("mail was re-delivered on the next launch:\n%s", again)
	}
}

// Nothing to say means nothing is added, so a caller can concatenate blind.
func TestPrependForAgent_UnchangedWhenNoMail(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	if got := PrependForAgent("claude-opus5", "do the thing"); got != "do the thing" {
		t.Fatalf("prompt was modified with no mail pending: %q", got)
	}
	if got := PrependForAgent("", "do the thing"); got != "do the thing" {
		t.Fatalf("an empty agent name must be a no-op: %q", got)
	}
}

func TestPrependForAgent_PutsMailBeforeThePrompt(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("ycode-glm-5.2"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "ycode-glm-5.2", Body: "gate is red"}); err != nil {
		t.Fatal(err)
	}
	got := PrependForAgent("ycode-glm-5.2", "fix the parser")
	if !strings.Contains(got, "gate is red") || !strings.Contains(got, "fix the parser") {
		t.Fatalf("both the mail and the prompt must survive:\n%s", got)
	}
	if strings.Index(got, "gate is red") > strings.Index(got, "fix the parser") {
		t.Fatal("mail must come BEFORE the prompt — it is context for the work, not a footnote")
	}
}

func TestRegisterRefsMBFound(t *testing.T) {
	busInTempHome(t)
	seq, err := PostMessageSeq(Post{From: "tester", Topic: "announce", Body: "first line\nsecond line"})
	if err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("mb:" + strconv.FormatInt(seq, 10))
	if err != nil {
		t.Fatal(err)
	}
	if n.Ref != "mb:1" || n.Title != "first line" || n.Status != "posted" || n.Where != BoardDir() {
		t.Fatalf("node = %+v", n)
	}
	if n.Open != "bashy mb show 1" {
		t.Fatalf("open = %q", n.Open)
	}
}

func TestRegisterRefsMBNotFound(t *testing.T) {
	busInTempHome(t)
	g := ref.NewRegistry()
	RegisterRefs(g)
	_, err := g.Resolve("mb:404")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegisterRefsMBFindsArchivedPost(t *testing.T) {
	busInTempHome(t)
	p := Post{SchemaVersion: BoardSchema, Seq: 12, At: "2026-09-14T00:00:00Z", From: "tester", Body: "rotated post"}
	writeJSONL(t, filepath.Join(BoardDir(), "archive", "2026-09.jsonl"), p)

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("mb:12")
	if err != nil {
		t.Fatal(err)
	}
	if n.Ref != "mb:12" || n.Title != "rotated post" {
		t.Fatalf("node = %+v", n)
	}
}

func TestRegisterRefsBusFound(t *testing.T) {
	busInTempHome(t)
	if err := room.Emit(room.Event{Type: room.EventNotify, Topic: "gate", Body: "green"}); err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("bus:1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Ref != "bus:1" || n.Title != "notify gate - green" || n.Where != room.Dir() {
		t.Fatalf("node = %+v", n)
	}
	if n.Open != "bashy bus watch --json --from 1" {
		t.Fatalf("open = %q", n.Open)
	}
}

func TestRegisterRefsBusNotFound(t *testing.T) {
	busInTempHome(t)
	g := ref.NewRegistry()
	RegisterRefs(g)
	_, err := g.Resolve("bus:404")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegisterRefsBusFindsArchivedEvent(t *testing.T) {
	busInTempHome(t)
	e := room.Event{Seq: 14, TS: "2026-09-14T00:00:00Z", Type: room.EventNotify, Topic: "archive", Body: "rotated event"}
	writeJSONL(t, filepath.Join(room.Dir(), "archive", "2026-09.jsonl"), e)

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("bus:14")
	if err != nil {
		t.Fatal(err)
	}
	if n.Ref != "bus:14" || n.Title != "notify archive - rotated event" {
		t.Fatalf("node = %+v", n)
	}
}

// TestMBShowPrintsOnePost: `mb show <seq>` is the single-record read behind an
// [[mb:N]] citation — the post, its ref, and NOTHING resolved: a citation inside
// the body is define's job, because the board cannot see kb/todo without a
// second copy of the link grammar.
func TestMBShowPrintsOnePost(t *testing.T) {
	busInTempHome(t)
	seq, err := PostMessageSeq(Post{From: "tester", Topic: "cite", Body: "read [[kb:missing-page]] then [[meet:room-1]]"})
	if err != nil {
		t.Fatal(err)
	}

	cmd := NewMessageBoardCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", strconv.FormatInt(seq, 10)})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"mb:" + strconv.FormatInt(seq, 10), "read [[kb:missing-page]] then [[meet:room-1]]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mb show missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dangling") || strings.Contains(got, "external") {
		t.Fatalf("mb show must not resolve citations (that is define's job):\n%s", got)
	}

	out.Reset()
	cmd = NewMessageBoardCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", strconv.FormatInt(seq, 10), "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"ref": "mb:`+strconv.FormatInt(seq, 10)+`"`) {
		t.Fatalf("mb show --json lacks the ref field:\n%s", out.String())
	}
}

func writeJSONL(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// READING MARKS, IT DOES NOT DELETE. An inbox that erases what it shows you
// cannot answer "what was I told, and when" — the question that comes up when a
// fleet run goes wrong. The room timeline keeps every message forever; the
// per-agent view must not disagree with it.
func TestInbox_ReadingMarksAndRetains(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("codex-gpt5.6-sol"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "codex-gpt5.6-sol", Body: "roster changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveFor("codex-gpt5.6-sol"); err != nil {
		t.Fatal(err)
	}

	unread, err := UnreadPending("codex-gpt5.6-sol")
	if err != nil || len(unread) != 1 {
		t.Fatalf("unread = %d, err = %v; want 1", len(unread), err)
	}
	if err := MarkRead("codex-gpt5.6-sol", unread[0].Seq); err != nil {
		t.Fatal(err)
	}

	// Gone from unread...
	if again, _ := UnreadPending("codex-gpt5.6-sol"); len(again) != 0 {
		t.Fatalf("a read message must not come back as unread, got %d", len(again))
	}
	// ...but STILL THERE, with a stamp.
	all, err := ReadPending("codex-gpt5.6-sol")
	if err != nil || len(all) != 1 {
		t.Fatalf("history lost: %d records, err %v", len(all), err)
	}
	if all[0].Body != "roster changed" {
		t.Fatalf("body was not retained: %+v", all[0])
	}
	if all[0].Unread() || all[0].ReadAt == "" {
		t.Fatalf("read_at was not stamped: %+v", all[0])
	}
}

// Re-reading must not rewrite when the agent FIRST saw something.
func TestInbox_MarkReadIsIdempotent(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "claude-opus5", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveFor("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	if err := MarkRead("claude-opus5", 1<<62); err != nil {
		t.Fatal(err)
	}
	first, _ := ReadPending("claude-opus5")
	if err := MarkRead("claude-opus5", 1<<62); err != nil {
		t.Fatal(err)
	}
	second, _ := ReadPending("claude-opus5")
	if len(first) != 1 || len(second) != 1 || first[0].ReadAt != second[0].ReadAt {
		t.Fatalf("re-reading changed the first-seen stamp: %q -> %q", first[0].ReadAt, second[0].ReadAt)
	}
}

// BROADCAST. A post with no recipient reaches everybody because every default
// subscription joins the board room — Matches accepts on Room as readily as on
// To. Without that membership a recipient-less post would match nothing and
// reach nobody while reporting success.
func TestBoard_BroadcastReachesEveryone(t *testing.T) {
	busInTempHome(t)
	fleet := []string{"claude-opus5", "codex-gpt5.6-sol", "ycode-glm-5.2"}
	if _, err := ReconcileSubscriptions(fleet); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{
		Type: room.EventNotify, Room: BoardRoom, Topic: "mb", Body: "sh/ is frozen",
	}); err != nil {
		t.Fatal(err)
	}
	for _, who := range fleet {
		n, err := ResolveFor(who)
		if err != nil || n != 1 {
			t.Fatalf("%s resolved %d (err %v); a broadcast must reach every board member", who, n, err)
		}
	}
}

// A subscription written before the board existed has no Room and would
// silently miss every broadcast. Reconcile repairs that — and touches NOTHING
// else, because InterruptFrom and MaxPerMin are operator policy.
func TestBoard_ReconcileJoinsLegacySubsWithoutClobberingPolicy(t *testing.T) {
	busInTempHome(t)
	legacy := Subscription{
		Subscriber:    "claude-opus5",
		To:            "claude-opus5",
		InterruptFrom: []string{"steward"},
		MaxPerMin:     11,
	}
	if err := SaveSubscription(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSubscriptions([]string{"claude-opus5"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Room != BoardRoom {
		t.Fatalf("legacy subscription was not joined to the board: %+v", got)
	}
	if len(got.InterruptFrom) != 1 || got.InterruptFrom[0] != "steward" || got.MaxPerMin != 11 {
		t.Fatalf("operator policy was clobbered by the repair: %+v", got)
	}
}

// NO PRIOR SETUP. Posting to a name with no subscription must still reach it:
// Matches needs a stored subscription, so without an auto-open the post reports
// success and reaches nobody — the exit-0 lie this package exists to remove.
func TestBoard_PostToAnUnknownNameStillArrives(t *testing.T) {
	busInTempHome(t)
	if _, err := LoadSubscription("never-seen-before"); err == nil {
		if s, _ := LoadSubscription("never-seen-before"); s.Subscriber != "" {
			t.Fatal("precondition: this name must start with no inbox")
		}
	}
	// What `mb send` does: open the inbox, THEN publish.
	if _, err := EnsureSubscription("never-seen-before"); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{
		Type: room.EventNotify, To: "never-seen-before", Body: "hello stranger",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := ResolveFor("never-seen-before")
	if err != nil || n != 1 {
		t.Fatalf("resolved %d (err %v); a post to a new name must arrive", n, err)
	}
}

// ORDER IS LOAD-BEARING. A new inbox opens at the timeline head, so opening it
// AFTER the post would leave the cursor at or past the message and deliver
// nothing.
func TestBoard_InboxMustOpenBeforeThePostIsAppended(t *testing.T) {
	busInTempHome(t)
	if err := room.Emit(room.Event{Type: room.EventNotify, To: "late-comer", Body: "sent first"}); err != nil {
		t.Fatal(err)
	}
	// Inbox opened after the fact — the wrong order, shown failing.
	if _, err := EnsureSubscription("late-comer"); err != nil {
		t.Fatal(err)
	}
	if n, _ := ResolveFor("late-comer"); n != 0 {
		t.Fatalf("resolved %d; an inbox opened at the head must not replay what predates it", n)
	}
}
