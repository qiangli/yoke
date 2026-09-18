package bus

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// The gate is what stops a 1 s inbox ticker from becoming a full core: the
// full read happens on the first tick, when the fingerprint moves, on the
// periodic rescan, when the fingerprint is unavailable, and after a Retry —
// and at no other time.
func TestPollGateReadsOnlyOnChangeRescanOrRetry(t *testing.T) {
	var sum uint64 = 1
	ok := true
	g := NewPollGate(func() (uint64, bool) { return sum, ok }, time.Minute)
	now := time.Unix(1_700_000_000, 0)

	read, s, o := g.Due(now)
	if !read {
		t.Fatal("first tick must read: nothing has been sampled yet")
	}
	g.Commit(s, o, now)

	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		if read, _, _ := g.Due(now); read {
			t.Fatalf("tick %d: unchanged fingerprint must not read", i)
		}
	}

	sum = 2
	now = now.Add(time.Second)
	read, s, o = g.Due(now)
	if !read {
		t.Fatal("a moved fingerprint must read")
	}
	g.Commit(s, o, now)
	if read, _, _ := g.Due(now.Add(time.Second)); read {
		t.Fatal("committed sample must not re-read")
	}

	if read, _, _ := g.Due(now.Add(time.Minute)); !read {
		t.Fatal("the full rescan must read even when nothing moved")
	}

	g.Retry()
	if read, _, _ := g.Due(now.Add(2 * time.Second)); !read {
		t.Fatal("Retry must force the next tick to read")
	}
	g.Commit(sum, true, now.Add(2*time.Second))

	ok = false
	if read, _, _ := g.Due(now.Add(3 * time.Second)); !read {
		t.Fatal("an unavailable fingerprint must fail open and read")
	}
}

// StoreFingerprint is a stat over the stores an agent's inbox reads. It must
// be stable while nothing is written, and move when the timeline or the
// agent's own pending buffer grows — the two writes that carry mail.
func TestStoreFingerprintTracksTimelineAndPending(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	t.Setenv("BASHY_MB_DIR", filepath.Join(dir, "mb"))
	agent := "fp-agent"

	a, ok := StoreFingerprint(agent)
	if !ok {
		t.Fatal("fingerprint over an empty store must be available (missing files are a known state)")
	}
	b, _ := StoreFingerprint(agent)
	if a != b {
		t.Fatal("fingerprint must be stable while nothing is written")
	}

	if err := room.Emit(room.Event{Type: room.EventNotify, Principal: "someone", To: agent, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	c, _ := StoreFingerprint(agent)
	if c == a {
		t.Fatal("a timeline append must move the fingerprint")
	}

	// Coarse mtimes: a same-second append still changes the size, which the
	// fingerprint also hashes. Force the size path explicitly.
	if err := AppendPending(agent, Pending{SchemaVersion: SchemaVersion, Seq: 1, Body: "queued"}); err != nil {
		t.Fatal(err)
	}
	d, _ := StoreFingerprint(agent)
	if d == c {
		t.Fatal("a pending-buffer append must move the fingerprint")
	}

	// The gate composes: after a commit, the next tick over the same bytes is
	// a no-read.
	g := NewInboxPollGate(agent)
	read, s, o := g.Due(time.Now())
	if !read {
		t.Fatal("first tick reads")
	}
	g.Commit(s, o, time.Now())
	if read, _, _ := g.Due(time.Now()); read {
		t.Fatal("nothing moved: the inbox gate must not read")
	}
	if InboxFingerprint != nil {
		t.Fatal("test assumes the default (unwired) fingerprint")
	}
	_ = os.Remove(filepath.Join(dir, "timeline.jsonl"))
	if read, _, _ := g.Due(time.Now()); !read {
		t.Fatal("a removed timeline is a change (rewrite/archive) and must read")
	}
}
