package bus

import (
	"encoding/binary"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// InboxFingerprint is the change gate for every in-process inbox poller (a
// chat session's relay, the bus sidecar). It answers one question cheaply: has
// anything an agent's inbox reads POSSIBLY changed since the last sample? A
// poller reads fully only when the answer is yes, or on its periodic full
// rescan.
//
// The host may replace it. Bashy wires the same fsnotify generation counter its
// `inbox --watch` already uses, which also covers the board and meet stores the
// host's PrepareTurnInbox hook reads — the rule is that whoever adds an inbox
// source owns its fingerprint. Nil (a standalone coreutils process, whose
// preamble is bus-only) falls back to StoreFingerprint.
//
// Why a gate and not a cheaper read: room.Timeline(0) is a full parse of the
// host timeline since the archive watermark, and SnapshotInbox calls it twice.
// On a host with a 27 MB / 246k-event timeline one unconditional poll cost
// ~435 ms of the coreutils half alone and ~3.4 s with the host's board+meet
// scan — inside a 1 s ticker. A ticker whose body outlasts its period fires
// back-to-back, so an idle managed session burned a full core while its agent
// sat at 2% (coreutils story #127). The Sprint 138 watcher fix stopped exactly
// this in `bashy inbox --watch`; the relay never received it.
var InboxFingerprint func(agent string) (uint64, bool)

func inboxFingerprint(agent string) (uint64, bool) {
	if InboxFingerprint != nil {
		return InboxFingerprint(agent)
	}
	return StoreFingerprint(agent)
}

// StoreFingerprint hashes the metadata (size and mtime) of every bus store an
// agent's inbox read touches: the room timeline, the agent's pending buffer,
// cursor and subscription. Constant cost — a handful of stats, no reads.
//
// It fails OPEN: a stat error other than not-exist reports ok=false, and a
// gate treats that as "read fully". A missing file is a known state (the agent
// has no pending buffer yet), not an error.
func StoreFingerprint(agent string) (uint64, bool) {
	f := storeFingerprinter{h: fnv.New64a(), ok: true}
	f.file(filepath.Join(room.Dir(), "timeline.jsonl"))
	for _, path := range []func(string) (string, error){pendingPath, cursorPath, subsPath} {
		p, err := path(agent)
		if err != nil {
			f.ok = false
			continue
		}
		f.file(p)
	}
	return f.h.Sum64(), f.ok
}

// TimelineFingerprint hashes only the timeline and the subscription set — what
// the sidecar reads. Same cost model and failure mode as StoreFingerprint.
func TimelineFingerprint() (uint64, bool) {
	f := storeFingerprinter{h: fnv.New64a(), ok: true}
	f.file(filepath.Join(room.Dir(), "timeline.jsonl"))
	f.file(filepath.Join(room.Dir(), subsDir)) // dir mtime moves on add/remove
	return f.h.Sum64(), f.ok
}

type storeFingerprinter struct {
	h interface {
		Write([]byte) (int, error)
		Sum64() uint64
	}
	ok bool
}

func (f *storeFingerprinter) file(path string) {
	_, _ = f.h.Write([]byte(path))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = f.h.Write([]byte{0})
			return
		}
		f.ok = false
		return
	}
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], uint64(info.Size()))
	binary.LittleEndian.PutUint64(b[8:], uint64(info.ModTime().UnixNano()))
	_, _ = f.h.Write(b[:])
}

// DefaultFullRescan bounds how long a poller trusts its fingerprint. The stores
// are durable and every read is idempotent, so the periodic full read is the
// correctness backstop for a stale fingerprint (an mtime that did not tick, a
// host notifier that missed an event), not the delivery path.
const DefaultFullRescan = 30 * time.Second

// PollGate decides, per tick, whether a poller must read its sources fully.
//
// The fingerprint is sampled BEFORE the read and committed AFTER it, so a write
// that lands during the read changes the next sample and cannot fall into the
// read/commit race. A read that could not be completed (delivery refused while
// a transport is busy) must call Retry, which forgets the sample so the next
// tick reads again — the sources did not change, but the poller's obligation
// did not go away either.
type PollGate struct {
	fingerprint func() (uint64, bool)
	fullRescan  time.Duration
	sampled     bool
	sum         uint64
	lastFull    time.Time
}

// NewPollGate builds a gate over a fingerprint. fullRescan <= 0 means
// DefaultFullRescan.
func NewPollGate(fingerprint func() (uint64, bool), fullRescan time.Duration) *PollGate {
	if fullRescan <= 0 {
		fullRescan = DefaultFullRescan
	}
	return &PollGate{fingerprint: fingerprint, fullRescan: fullRescan}
}

// NewInboxPollGate is the gate for an agent's unified inbox, over
// InboxFingerprint (host-wired) or StoreFingerprint.
func NewInboxPollGate(agent string) *PollGate {
	return NewPollGate(func() (uint64, bool) { return inboxFingerprint(agent) }, 0)
}

// Due reports whether the poller must read now, and returns the sample to
// Commit afterwards. It reads on the first tick, whenever the fingerprint moved,
// whenever the fingerprint is unavailable (fail open), and on every fullRescan.
func (g *PollGate) Due(now time.Time) (read bool, sum uint64, ok bool) {
	sum, ok = g.fingerprint()
	switch {
	case !ok || !g.sampled:
		return true, sum, ok
	case sum != g.sum:
		return true, sum, ok
	case now.Sub(g.lastFull) >= g.fullRescan:
		return true, sum, ok
	default:
		return false, sum, ok
	}
}

// Commit records a completed read of the sources sampled at sum.
func (g *PollGate) Commit(sum uint64, ok bool, now time.Time) {
	g.sampled = ok
	g.sum = sum
	g.lastFull = now
}

// Retry forgets the last sample so the next tick reads regardless.
func (g *PollGate) Retry() { g.sampled = false }
