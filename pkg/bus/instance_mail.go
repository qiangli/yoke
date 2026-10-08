// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package bus

// THE REAL MAIL SOURCE FOR A RETIRING INSTANCE.
//
// fleet.Retire takes a fleet.MailSource rather than reading the bus itself, so
// that pkg/fleet stays a leaf, and it REFUSES a nil one
// (fleet.ErrMailSourceRequired) rather than writing an empty archive — an
// archive that reports preserved mail and holds none is indistinguishable from
// an instance that genuinely received nothing. This file is the implementation
// that seam exists for. It lives in pkg/bus because the bus owns the records
// and their format, and because the import already runs this way (bus → fleet).

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// InstanceMail is the fleet.MailSource over this host's bus.
//
// It collects an instance's PERSONAL mail — everything addressed to
// instance/<uuid> — from both places a record can be sitting:
//
//   - the pending buffer, which is the materialized per-agent mailbox, and
//   - the durable room timeline, which keeps directed posts whether or not a
//     subscription had materialized them yet.
//
// Both, because either alone loses records. The pending buffer can hold
// something the timeline rotated away (see archive.go), and the timeline holds
// addressed backlog that predates the subscription — the same reconciliation
// SnapshotInbox performs for a live read, done here WITHOUT side effects:
// retiring must not create a subscription, append to a buffer or advance a
// cursor. Archiving is evidence collection, and evidence collection that
// mutates its subject is not evidence.
//
// READ AND UNREAD alike are archived. "Reading marks, it does not delete" is
// the bus's rule, and an archive of only the unread would answer "what was
// this conversation never told" instead of "what was it told" — the question
// an operator actually has after a run goes wrong.
func InstanceMail(inst fleet.Instance) ([]string, error) {
	addr := inst.MailAddress()
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("bus: instance has no mail address; it has no UUID")
	}

	type record struct {
		seq  int64
		line string
	}
	var records []record
	seen := map[int64]bool{}

	pending, err := ReadPending(addr)
	if err != nil {
		return nil, fmt.Errorf("bus: read pending mail for %s: %w", addr, err)
	}
	for _, p := range pending {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("bus: encode pending mail for %s: %w", addr, err)
		}
		records = append(records, record{seq: p.Seq, line: string(b)})
		seen[p.Seq] = true
	}

	events, err := watchTimeline(0)
	if err != nil {
		return nil, fmt.Errorf("bus: read timeline for %s: %w", addr, err)
	}
	filter := eventFilter{to: addr}
	for _, e := range events {
		if !filter.match(e) || seen[e.Seq] {
			continue
		}
		// Rendered in the SAME envelope as the buffered records, so the
		// archive is one readable format rather than two that a later reader
		// has to sniff. Delivery is what it actually was: the record reached
		// the timeline and was never materialized into the buffer.
		p := Pending{
			SchemaVersion: SchemaVersion,
			Seq:           e.Seq,
			TS:            e.TS,
			Principal:     e.Principal,
			Topic:         e.Topic,
			To:            e.To,
			Room:          e.Room,
			Body:          e.Body,
			Delivery:      DeliveryQueued,
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("bus: encode timeline mail for %s: %w", addr, err)
		}
		records = append(records, record{seq: e.Seq, line: string(b)})
		seen[e.Seq] = true
	}

	sort.SliceStable(records, func(i, j int) bool { return records[i].seq < records[j].seq })
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.line)
	}
	return out, nil
}

// instanceMailSource is the compile-time assertion that this function is the
// seam fleet declared. A signature drift would otherwise only surface at the
// one call site that retires an instance.
var instanceMailSource fleet.MailSource = InstanceMail

// InstanceMailAddressed reports whether a timeline event is personal mail for
// an instance. Exported for the delivery work in #1109, which needs the same
// predicate and must not reimplement it.
func InstanceMailAddressed(e room.Event, inst fleet.Instance) bool {
	addr := inst.MailAddress()
	if strings.TrimSpace(addr) == "" {
		return false
	}
	return eventFilter{to: addr}.match(e)
}
