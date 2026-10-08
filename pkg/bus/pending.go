package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/room"
)

const pendingDir = "pending"

// Pending is one pre-resolved notification waiting for its agent.
//
// "Pre-resolved" is the point of the sidecar. The agent does not evaluate
// subscriptions, match topics, check principals or apply rate limits — all of
// that happened off its critical path, and what reaches it is a short list of
// things already determined to be its business.
type Pending struct {
	SchemaVersion string `json:"schema_version"`
	Seq           int64  `json:"seq"`
	TS            string `json:"ts"`
	Principal     string `json:"principal,omitempty"`
	Topic         string `json:"topic,omitempty"`
	To            string `json:"to,omitempty"`
	Room          string `json:"room,omitempty"`
	Body          string `json:"body,omitempty"`
	// Delivery records the tier this was ACTUALLY delivered at, which is not
	// always the tier the publisher asked for — see Demoted.
	Delivery string `json:"delivery"`
	// Demoted explains why an interrupt was downgraded to queued (not authorized,
	// rate-limited, or no live instance to steer). It is recorded rather than
	// silently applied so an operator can see that the bus withheld urgency, and
	// why: a governance decision nobody can observe is indistinguishable from a bug.
	Demoted string `json:"demoted,omitempty"`
	// ReadAt stamps when this was shown to its agent. EMPTY MEANS UNREAD, which
	// is the only state that matters for delivery.
	//
	// Reading MARKS, it does not delete. A message is history the moment it is
	// sent: an inbox that erases what it shows you cannot answer "what was I
	// told, and when" — the question that actually comes up after a fleet run
	// goes wrong. The append-only room timeline already keeps every message
	// forever; this keeps the per-agent VIEW of them just as long, so the two
	// cannot disagree.
	ReadAt string `json:"read_at,omitempty"`
}

// Unread reports whether this message has not yet been shown to its agent.
func (p Pending) Unread() bool { return strings.TrimSpace(p.ReadAt) == "" }

func pendingPath(subscriber string) (string, error) {
	name := safeName.ReplaceAllString(subscriber, "_")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "anonymous"
	}
	dir := filepath.Join(room.Dir(), pendingDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("bus: creating %s: %w", dir, err)
	}
	return filepath.Join(dir, name+".jsonl"), nil
}

// pendingLockPath is the cross-process lock serializing every writer of the
// pending buffers. Nothing ever lists this directory (buffers are addressed
// by subscriber name), so the lock can live beside them like the board's.
func pendingLockPath() string {
	dir := filepath.Join(room.Dir(), pendingDir)
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, ".lock")
}

// withPendingLock runs fn holding a short, best-effort cross-process lock.
//
// Best-effort deliberately, matching withBoardLock: a lock that could fail a
// delivery (a notification never buffered because maintenance could not get
// a lock) would be a worse defect than the rare race it prevents. On any
// acquisition failure fn still runs, unlocked — the pre-existing behavior.
func withPendingLock(intent string, fn func()) {
	lock, err := lockfile.AcquireWithin(pendingLockPath(), 2*time.Second, lockfile.Holder{
		Name: "bus-pending", PID: os.Getpid(), Intent: intent,
	})
	if err == nil {
		defer lock.Release()
	}
	fn()
}

// AppendPending adds to a subscriber's buffer.
//
// Append-only, one JSON object per line: the sidecar writes while the agent may
// be reading, and an append is the one filesystem operation that cannot hand a
// reader a half-written record.
//
// The append holds the pending lock, which is what keeps it from racing a
// concurrent MarkRead/ClearPending rewrite of this same file: without it, a
// rewrite landing between that path's read and its write silently drops this
// append. Locking is what makes the rewrite-while-appending pair safe; the
// O_APPEND alone only keeps concurrent appends from interleaving each other.
func AppendPending(subscriber string, p Pending) error {
	var werr error
	withPendingLock("append", func() {
		path, err := pendingPath(subscriber)
		if err != nil {
			werr = err
			return
		}
		b, err := json.Marshal(p)
		if err != nil {
			werr = err
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			werr = fmt.Errorf("bus: writing the pending buffer: %w", err)
			return
		}
		_, werr = f.Write(append(b, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
	})
	return werr
}

// ReadPending returns a subscriber's buffer.
func ReadPending(subscriber string) ([]Pending, error) {
	path, err := pendingPath(subscriber)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Pending
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p Pending
		if json.Unmarshal([]byte(line), &p) != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// writePending rewrites a subscriber's buffer. Used by the mark and clear paths;
// the append path never calls it, so a concurrent sidecar append is never
// serialized through a full rewrite.
//
// Callers must hold withPendingLock across their read-modify-write: the lock
// is what keeps a sidecar append landing mid-rewrite from being silently
// dropped. The rewrite itself goes through tmp+rename, so a concurrent
// lock-free reader never sees a truncated file — only the old buffer or the
// new one.
func writePending(subscriber string, items []Pending) error {
	path, err := pendingPath(subscriber)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		return nil
	}
	var b strings.Builder
	for _, p := range items {
		line, merr := json.Marshal(p)
		if merr != nil {
			return merr
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

// MarkRead stamps everything through throughSeq as read, RETAINING it.
//
// This is what a read does now. ClearPending (below) still exists and still
// deletes, because a caller may genuinely want to drop a buffer — but nothing
// on the read path uses it, and nothing should: an inbox that erases what it
// shows you has no history, and history is the whole reason to keep a log.
//
// Already-read entries are left exactly as they were, so re-reading does not
// rewrite a timestamp and lose when the agent FIRST saw something.
func MarkRead(subscriber string, throughSeq int64) error {
	var rerr error
	// The read, the stamping, and the rewrite are one critical section:
	// anything appended after the read must still be in the file the
	// rewrite produces, or it is silently lost.
	withPendingLock("mark-read", func() {
		all, err := ReadPending(subscriber)
		if err != nil {
			rerr = err
			return
		}
		now := nowRFC()
		changed := false
		for i := range all {
			if all[i].Seq <= throughSeq && all[i].Unread() {
				all[i].ReadAt = now
				changed = true
			}
		}
		if !changed {
			return
		}
		rerr = writePending(subscriber, all)
	})
	return rerr
}

// MarkPendingRead marks exactly one materialized timeline record as read.
// Unlike MarkRead it does not consume earlier records merely because a
// higher-priority later record was admitted first.
func MarkPendingRead(subscriber string, seq int64) error {
	var rerr error
	// Same critical section as MarkRead: the single-record stamp must not
	// drop a sidecar append landing mid-rewrite either.
	withPendingLock("mark-pending-read", func() {
		all, err := ReadPending(subscriber)
		if err != nil {
			rerr = err
			return
		}
		now := nowRFC()
		changed := false
		for i := range all {
			if all[i].Seq == seq && all[i].Unread() {
				all[i].ReadAt = now
				changed = true
			}
		}
		if !changed {
			return
		}
		rerr = writePending(subscriber, all)
	})
	return rerr
}

// UnreadPending returns only what the agent has not been shown.
func UnreadPending(subscriber string) ([]Pending, error) {
	all, err := ReadPending(subscriber)
	if err != nil {
		return nil, err
	}
	out := make([]Pending, 0, len(all))
	for _, p := range all {
		if p.Unread() {
			out = append(out, p)
		}
	}
	return out, nil
}

// ClearPending empties a subscriber's buffer up to and including seq.
//
// Bounded by a sequence number rather than truncating wholesale, because the
// sidecar may append between the agent's read and its clear. Truncating the file
// would silently discard whatever arrived in that window — a dropped
// notification, which leaves an agent acting on stale assumptions and is the one
// outcome this whole design refuses.
func ClearPending(subscriber string, throughSeq int64) error {
	var rerr error
	// One critical section like the mark paths: the comment on the filter
	// already refuses wholesale truncation, and the lock is what makes that
	// bound hold against an append landing between the read and the rewrite.
	withPendingLock("clear", func() {
		all, err := ReadPending(subscriber)
		if err != nil {
			rerr = err
			return
		}
		var keep []Pending
		for _, p := range all {
			if p.Seq > throughSeq {
				keep = append(keep, p)
			}
		}
		if rerr = writePending(subscriber, keep); rerr != nil {
			return
		}
	})
	return rerr
}

// FormatPending renders a buffer as the block injected at a turn boundary.
//
// Deliberately terse. This text is prepended to an agent's context, so every
// line costs attention that would otherwise go to the task — the same reason the
// design insists delivery be sparse. One line per notification, the sender and
// topic first so relevance is judgeable without reading the body.
func FormatPending(items []Pending) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Notifications (%d)\n\n", len(items))
	for _, p := range items {
		fmt.Fprintf(&b, "- **%s**", p.Topic)
		if p.Principal != "" {
			fmt.Fprintf(&b, " from `%s`", p.Principal)
		}
		if p.Delivery == DeliveryInterrupt {
			b.WriteString(" **[urgent]**")
		}
		fmt.Fprintf(&b, " — %s\n", p.Body)
	}
	return b.String()
}
