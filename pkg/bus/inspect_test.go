package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func notify(t *testing.T, from, to, topic, body string) {
	t.Helper()
	if err := room.Notify(room.Event{
		Type: room.EventNotify, Principal: from, To: to, Topic: topic, Body: body,
	}); err != nil {
		t.Fatal(err)
	}
}

// fingerprint hashes every file under dir, path and content both.
//
// A modtime comparison would not do: the failure this guards against is a
// rewrite with identical bytes at a new position (MarkRead stamping a ReadAt, a
// cursor advancing), and it is the CONTENT that must not move.
func fingerprint(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// THE invariant. An observer's page must not consume anybody's mail.
//
// This is not a hypothetical: SnapshotInbox — the read path `bashy inbox` uses,
// and the obvious thing to have reused here — calls EnsureSubscription and
// AppendPending, so inspecting an agent through it would create that agent's
// inbox and materialize its backlog. A human opening a browser tab would then
// have changed the state of a fleet they meant only to look at.
func TestInspectionWritesNothing(t *testing.T) {
	dir := isolate(t)
	FleetNames = func() []string { return []string{"cairn", "lintel"} }
	t.Cleanup(func() { FleetNames = nil })

	notify(t, "operator", "cairn", "sprint", "pick up #12")
	notify(t, "cairn", "lintel", "gate", "86/86")
	notify(t, "lintel", "operator", "done", "merged")

	before := fingerprint(t, dir)

	holders, err := InspectInboxes("operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range holders {
		if _, err := InspectInbox(h.Name, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	// A name nobody has ever addressed must also be inert: the pending and
	// cursor paths both MkdirAll, and creating a directory for a name that has
	// no mail is still a write.
	if _, err := InspectInbox("never-seen", "operator"); err != nil {
		t.Fatal(err)
	}

	after := fingerprint(t, dir)
	if len(before) != len(after) {
		t.Fatalf("inspection changed the file set: %d files before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("inspection rewrote %s", path)
		}
	}
}

func TestInspectInboxesListsEveryAddressableName(t *testing.T) {
	isolate(t)
	FleetNames = func() []string { return []string{"cairn"} }
	t.Cleanup(func() { FleetNames = nil })

	// An ephemeral worker: addressed, never registered. A roster built from the
	// catalog alone would hide it, and its backlog with it.
	notify(t, "cairn", "codex-w12", "issue", "take #12")
	if err := SaveSubscription(Subscription{Subscriber: "keystone", To: "keystone"}); err != nil {
		t.Fatal(err)
	}

	holders, err := InspectInboxes("operator")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]InboxHolder{}
	var names []string
	for _, h := range holders {
		got[h.Name] = h
		names = append(names, h.Name)
	}
	sort.Strings(names)

	for name, want := range map[string]struct{ kind, source string }{
		"operator":  {InboxKindPerson, InboxHolderViewer},
		"cairn":     {InboxKindAgent, InboxHolderCatalog},
		"codex-w12": {InboxKindOther, InboxHolderAddressed},
		"keystone":  {InboxKindOther, InboxHolderSubscribed},
	} {
		h, ok := got[name]
		if !ok {
			t.Errorf("%s is missing from the roster %v", name, names)
			continue
		}
		if h.Kind != want.kind {
			t.Errorf("%s kind = %q, want %q", name, h.Kind, want.kind)
		}
		if !containsStr(h.Sources, want.source) {
			t.Errorf("%s sources = %v, want it to include %q", name, h.Sources, want.source)
		}
	}
	if h := got["codex-w12"]; h.Total != 1 || h.Unread != 1 {
		t.Errorf("codex-w12 = %d total / %d unread, want 1/1", h.Total, h.Unread)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Read status is REPORTED, never assumed. A materialized stamp and a drain
// cursor that merely passed an item are different facts, and an operator
// looking at a stalled agent needs to tell them apart.
func TestInspectInboxDistinguishesStampedFromCursorRead(t *testing.T) {
	isolate(t)

	notify(t, "operator", "cairn", "one", "first")
	notify(t, "operator", "cairn", "two", "second")
	notify(t, "operator", "cairn", "three", "third")

	events, err := room.Timeline(0)
	if err != nil || len(events) != 3 {
		t.Fatalf("timeline = %d events, %v", len(events), err)
	}

	// #1 is materialized AND stamped; the cursor is advanced past #2 without
	// materializing it, which is exactly what a bare `bus watch --drain` does.
	e := events[0]
	if err := AppendPending("cairn", Pending{
		SchemaVersion: SchemaVersion, Seq: e.Seq, TS: e.TS, Principal: e.Principal,
		Topic: e.Topic, To: e.To, Room: e.Room, Body: e.Body, Delivery: DeliveryQueued,
	}); err != nil {
		t.Fatal(err)
	}
	if err := MarkRead("cairn", 1); err != nil {
		t.Fatal(err)
	}
	if err := writeCursor("cairn", 2); err != nil {
		t.Fatal(err)
	}

	view, verr := InspectInbox("cairn", "operator")
	if verr != nil {
		t.Fatal(verr)
	}
	if len(view.Items) != 3 {
		t.Fatalf("cairn has %d items, want 3 (one materialized, two timeline-only)", len(view.Items))
	}
	if view.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", view.Cursor)
	}

	first, second, third := view.Items[0], view.Items[1], view.Items[2]
	if first.Source != InboxSourcePending || first.ReadAt == "" || !first.Read {
		t.Errorf("#1 = %+v, want a stamped pending record", first)
	}
	if second.Source != InboxSourceTimeline || second.ReadAt != "" || !second.Read || !second.PastCursor {
		t.Errorf("#2 = %+v, want timeline-only, unstamped, behind the cursor", second)
	}
	if third.Read || third.PastCursor {
		t.Errorf("#3 = %+v, want unread and ahead of the cursor", third)
	}
	if view.Unread != 1 {
		t.Errorf("unread = %d, want 1", view.Unread)
	}
}

// The materialized record and the timeline event are the SAME message. Showing
// both would double every notification an agent has actually been handed.
func TestInspectInboxDoesNotDoubleCountMaterializedMail(t *testing.T) {
	isolate(t)
	notify(t, "operator", "cairn", "sprint", "pick up #12")

	events, err := room.Timeline(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("timeline = %v, %v", events, err)
	}
	e := events[0]
	if err := AppendPending("cairn", Pending{
		SchemaVersion: SchemaVersion, Seq: e.Seq, TS: e.TS, Principal: e.Principal,
		Topic: e.Topic, To: e.To, Room: e.Room, Body: e.Body, Delivery: DeliveryQueued,
	}); err != nil {
		t.Fatal(err)
	}

	view, err := InspectInbox("cairn", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 {
		t.Fatalf("cairn has %d items, want 1 — the pending record REPRESENTS the timeline event", len(view.Items))
	}
	if view.Items[0].Source != InboxSourcePending {
		t.Errorf("kept the wrong copy: %+v", view.Items[0])
	}
}

// The snapshot is revalidated by STAT, not by a timer, and that is what makes
// it safe to hold: a quiet host pays one os.Stat, and a host that just took a
// message reports the snapshot stale rather than serving a number from before
// it. A TTL cache could not offer the second half.
func TestInboxSnapshotIsStaleOnlyWhenTheTimelineMoves(t *testing.T) {
	isolate(t)
	notify(t, "operator", "cairn", "one", "first")

	snap, err := ReadInboxes()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Stale() {
		t.Fatal("a snapshot of an untouched timeline reports stale; every read would re-parse")
	}
	view, err := snap.Of("cairn", "operator")
	if err != nil || len(view.Items) != 1 {
		t.Fatalf("cairn = %d items, %v", len(view.Items), err)
	}

	notify(t, "operator", "cairn", "two", "second")
	if !snap.Stale() {
		t.Fatal("the timeline grew and the snapshot still reports current — " +
			"a held snapshot would answer about mail that arrived before the message being asked about")
	}
	// And the OLD snapshot keeps answering consistently rather than half-updating:
	// it is a parse, not a live view, so it can be behind but never in conflict.
	if view, err := snap.Of("cairn", "operator"); err != nil || len(view.Items) != 1 {
		t.Fatalf("stale snapshot = %d items, %v; want the 1 it was read with", len(view.Items), err)
	}

	fresh, err := ReadInboxes()
	if err != nil {
		t.Fatal(err)
	}
	if view, err := fresh.Of("cairn", "operator"); err != nil || len(view.Items) != 2 {
		t.Fatalf("re-read = %d items, %v; want 2", len(view.Items), err)
	}
}

// A snapshot must not become a way to write. It is the same read path, so this
// pins the invariant at the reusable entry point too.
func TestInboxSnapshotWritesNothing(t *testing.T) {
	dir := isolate(t)
	notify(t, "operator", "cairn", "sprint", "pick up #12")

	before := fingerprint(t, dir)
	snap, err := ReadInboxes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Holders("operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Of("cairn", "operator"); err != nil {
		t.Fatal(err)
	}
	_ = snap.Stale()
	after := fingerprint(t, dir)

	if len(before) != len(after) {
		t.Fatalf("the snapshot changed the file set: %d before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("the snapshot rewrote %s", path)
		}
	}
}
