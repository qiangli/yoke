package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// AppendPending adds to a subscriber's buffer.
//
// Append-only, one JSON object per line: the sidecar writes while the agent may
// be reading, and an append is the one filesystem operation that cannot hand a
// reader a half-written record.
func AppendPending(subscriber string, p Pending) error {
	path, err := pendingPath(subscriber)
	if err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("bus: writing the pending buffer: %w", err)
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
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
	return os.WriteFile(path, []byte(b.String()), 0o600)
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
	all, err := ReadPending(subscriber)
	if err != nil {
		return err
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
		return nil
	}
	return writePending(subscriber, all)
}

// MarkPendingRead marks exactly one materialized timeline record as read.
// Unlike MarkRead it does not consume earlier records merely because a
// higher-priority later record was admitted first.
func MarkPendingRead(subscriber string, seq int64) error {
	all, err := ReadPending(subscriber)
	if err != nil {
		return err
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
		return nil
	}
	return writePending(subscriber, all)
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
	all, err := ReadPending(subscriber)
	if err != nil {
		return err
	}
	var keep []Pending
	for _, p := range all {
		if p.Seq > throughSeq {
			keep = append(keep, p)
		}
	}
	path, err := pendingPath(subscriber)
	if err != nil {
		return err
	}
	if len(keep) == 0 {
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		return nil
	}
	var b strings.Builder
	for _, p := range keep {
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
