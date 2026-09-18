package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	t.Setenv("BASHY_PRINCIPAL", "tester")
	// The send path's resolver fallback (resolveprincipal.go) reads the
	// fleet catalog and the observation stores; keep them hermetic.
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_AGENTS_DIR", "")
	t.Setenv("BASHY_PEOPLE_DIR", "")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_PEOPLE_PATH", "")
	return dir
}

// run drives the whole `bus` tree, so the subcommand wiring is exercised too —
// a test that called newPublishCmd directly would pass even if the parent never
// registered it.
func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewBusCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func publish(t *testing.T, args ...string) {
	t.Helper()
	if _, _, err := run(t, append([]string{"publish"}, args...)...); err != nil {
		t.Fatalf("publish %v: %v", args, err)
	}
}

// --- publish -------------------------------------------------------------

func TestPublishLandsOnTheTimeline(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "rebase", "first")

	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != room.EventNotify || e.Principal != "alice" || e.Topic != "build" {
		t.Errorf("event = %+v", e)
	}
	if e.Body != "rebase first" {
		t.Errorf("body = %q", e.Body)
	}
}

// bashy is pre-1.0 and unreleased until POSIX certification, so a retired flag
// is REMOVED rather than carried as a hidden alias. This pins the removal: a
// compatibility shim nobody pinned is one that quietly grows back.
func TestPublishPrincipalIsRemoved(t *testing.T) {
	isolate(t)
	if _, _, err := run(t, "publish", "--topic", "build", "--principal", "alice", "rebase"); err == nil {
		t.Fatal("--principal must be rejected; identity is --as")
	}
	cmd := NewBusCmd()
	pub, _, err := cmd.Find([]string{"publish"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := pub.Flags().Lookup("principal"); flag != nil {
		t.Fatalf("--principal must not be registered at all, got %#v", flag)
	}
}

func TestPublishSpacetimeMovementCarriesNoCoordinateValues(t *testing.T) {
	isolate(t)
	if err := PublishSpacetimeMovement([]string{"place.id", "net.same_lan"}); err != nil {
		t.Fatal(err)
	}
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	event := events[0]
	if event.Topic != TopicSpacetimeMovement || event.Principal != "spacetime" {
		t.Fatalf("movement event = %+v", event)
	}
	if event.Body != "network coordinate changed: place.id, net.same_lan" {
		t.Fatalf("movement body = %q", event.Body)
	}
}

func TestPublishSpacetimeMovementRejectsValues(t *testing.T) {
	isolate(t)
	raw := "ssid=Cafe WiFi Secret"
	if err := PublishSpacetimeMovement([]string{raw}); err == nil {
		t.Fatal("movement publisher accepted a raw coordinate value")
	}
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("rejected raw value reached timeline: %+v", events)
	}
}

// An unaddressed notification has no topic to match, no recipient to route to
// and no room to scope it. Accepting it would report success for something that
// cannot be delivered.
func TestUnaddressedPublishIsRefused(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "publish", "--as", "alice", "to nobody")
	if err == nil {
		t.Fatal("an unaddressed publish was accepted")
	}
	if !strings.Contains(err.Error(), "addressing is required") {
		t.Errorf("error should name the missing addressing: %v", err)
	}
	if events, _ := room.Timeline(0); len(events) != 0 {
		t.Errorf("a refused publish still wrote %d event(s)", len(events))
	}
}

func TestPrincipalIsRequired(t *testing.T) {
	isolate(t)
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "")
	if _, _, err := run(t, "publish", "--topic", "build", "unattributed"); err == nil {
		t.Fatal("a publish with no principal was accepted")
	}
	if events, _ := room.Timeline(0); len(events) != 0 {
		t.Errorf("a refused publish still wrote %d event(s)", len(events))
	}
}

// The envelope and the exit code must agree. The previous implementation
// returned enc.Encode(env) — nil when the encode succeeded — so every --json
// refusal printed "status":"error" and then exited 0.
func TestJSONRefusalStillFails(t *testing.T) {
	isolate(t)
	_, errOut, err := run(t, "publish", "--as", "alice", "--json", "unaddressed")
	if err == nil {
		t.Fatal("a --json refusal reported success")
	}
	var env PublishEnvelope
	if jerr := json.Unmarshal([]byte(errOut), &env); jerr != nil {
		t.Fatalf("stderr is not valid JSON: %v\n%s", jerr, errOut)
	}
	if env.Status != "error" || env.Error == "" {
		t.Errorf("envelope = %+v", env)
	}
	if !Reported(err) {
		t.Error("a --json refusal must be marked reported, or the front end prints a second copy into the JSON stream")
	}
}

// --- watch: filtering ----------------------------------------------------

func TestWatchDrainFiltersByAddressing(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "build msg")
	publish(t, "--topic", "deploy", "--as", "alice", "deploy msg")
	publish(t, "--to", "dev-2", "--as", "bob", "direct msg")

	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "build msg") {
		t.Errorf("drain missed its own topic:\n%s", out)
	}
	if strings.Contains(out, "deploy msg") || strings.Contains(out, "direct msg") {
		t.Errorf("drain delivered somebody else's notifications:\n%s", out)
	}
}

// Session bookkeeping (join/leave/steer) shares the timeline with notifications
// but is not addressed to anyone; a bus subscriber must not see it.
func TestWatchIgnoresNonNotifyEvents(t *testing.T) {
	isolate(t)
	if err := room.Emit(room.Event{Type: room.EventJoin, Actor: "someone"}); err != nil {
		t.Fatal(err)
	}
	publish(t, "--topic", "build", "--as", "alice", "real notification")

	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "someone") {
		t.Errorf("a join event leaked onto the bus:\n%s", out)
	}
	if !strings.Contains(out, "real notification") {
		t.Errorf("the notification was dropped:\n%s", out)
	}
}

// Watching with no filter and no --all is refused: it is almost always a
// mistake, and in drain mode it would advance a cursor over messages the caller
// never meant to claim.
func TestWatchRequiresAFilterOrAll(t *testing.T) {
	isolate(t)
	if _, _, err := run(t, "watch", "--drain"); err == nil {
		t.Error("watch with no filter and no --all was accepted")
	}
	publish(t, "--topic", "build", "--as", "alice", "msg")
	if _, _, err := run(t, "watch", "--all", "--drain", "--as", "s"); err != nil {
		t.Errorf("--all should permit an unfiltered watch: %v", err)
	}
}

// --- watch: drain semantics ----------------------------------------------

// Drain is "what have I not seen", not "what is on the bus". Draining twice
// must not re-deliver.
func TestDrainIsIncremental(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "first")

	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first") {
		t.Fatalf("first drain missed the message:\n%s", out)
	}

	out2, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "first") {
		t.Errorf("a second drain re-delivered an already-seen message:\n%s", out2)
	}

	publish(t, "--topic", "build", "--as", "alice", "second")
	out3, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out3, "second") {
		t.Errorf("drain missed a message published after the cursor:\n%s", out3)
	}
}

// A first drain must deliver the BACKLOG, not skip to the end. Starting at the
// end would silently swallow messages published for a subscriber before it first
// ran — which is the entire scenario the bus exists to prevent.
func TestFirstDrainDeliversTheBacklog(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "published before anyone watched")

	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "brand-new")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "published before anyone watched") {
		t.Errorf("a first drain skipped the backlog:\n%s", out)
	}
}

// Cursors are per-subscriber: one agent draining must not consume another's
// copy. That is the difference between a bus and a queue.
func TestCursorsArePerSubscriber(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "for everyone")

	if out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "agent-a"); err != nil || !strings.Contains(out, "for everyone") {
		t.Fatalf("agent-a drain: %v\n%s", err, out)
	}
	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "for everyone") {
		t.Error("agent-a's drain consumed agent-b's copy — that is a queue, not a bus")
	}
}

// The cursor advances over everything READ, not merely everything matched, or a
// narrowed filter would rewind and re-deliver.
func TestCursorAdvancesPastUnmatchedEvents(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "other", "--as", "alice", "not mine")
	publish(t, "--topic", "build", "--as", "alice", "mine")

	if _, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1"); err != nil {
		t.Fatal(err)
	}
	// Now widen the filter: the earlier unmatched event is behind the cursor and
	// must not reappear.
	out, _, err := run(t, "watch", "--all", "--drain", "--as", "sub1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "not mine") {
		t.Errorf("widening the filter rewound the cursor:\n%s", out)
	}
}

func TestDrainJSONIsNDJSON(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "one")
	publish(t, "--topic", "build", "--as", "bob", "two")

	out, _, err := run(t, "watch", "--topic", "build", "--drain", "--as", "sub1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), out)
	}
	for i, ln := range lines {
		var ev WatchEvent
		if jerr := json.Unmarshal([]byte(ln), &ev); jerr != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, jerr, ln)
		}
		if ev.SchemaVersion != SchemaVersion || ev.Topic != "build" || ev.Seq == 0 {
			t.Errorf("line %d = %+v", i, ev)
		}
	}
}

// --- cursor storage -------------------------------------------------------

// A subscriber name becomes a path segment, and it comes from a flag or the
// environment. It must not be able to escape the cursor directory.
func TestSubscriberNameCannotEscapeTheCursorDir(t *testing.T) {
	dir := isolate(t)
	cursors := filepath.Join(dir, cursorDir)

	for _, evil := range []string{"../../escape", "..", ".", "a/b/c", "../../../etc/passwd"} {
		if err := writeCursor(evil, 42); err != nil {
			t.Fatalf("writeCursor(%q): %v", evil, err)
		}
	}
	// Everything written must live directly inside the cursor directory.
	entries, err := os.ReadDir(cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no cursor files were written at all")
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("cursor name created a directory: %s", e.Name())
		}
		if strings.Contains(e.Name(), "/") || strings.HasPrefix(e.Name(), ".") {
			t.Errorf("unsafe cursor file name: %q", e.Name())
		}
	}
	// And nothing may have appeared beside the room dir.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Error("a cursor escaped the room directory")
	}
}

// A corrupt cursor rewinds rather than failing: re-delivering is recoverable,
// refusing to drain at all is not.
func TestCorruptCursorRewindsRatherThanFailing(t *testing.T) {
	isolate(t)
	path, err := cursorPath("sub1")
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(path, []byte("not a number"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	got, err := readCursor("sub1")
	if err != nil {
		t.Fatalf("a corrupt cursor returned an error instead of rewinding: %v", err)
	}
	if got != 0 {
		t.Errorf("cursor = %d, want 0", got)
	}
}

func TestResolveSubscriberFallsBackToPrincipal(t *testing.T) {
	isolate(t)
	if got := resolveSubscriber(""); got != "tester" {
		t.Errorf("resolveSubscriber(\"\") = %q, want the principal", got)
	}
	if got := resolveSubscriber("explicit"); got != "explicit" {
		t.Errorf("resolveSubscriber(\"explicit\") = %q", got)
	}
}
