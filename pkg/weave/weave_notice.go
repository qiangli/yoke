package weave

// Owner notices are lifecycle facts, not another coordination channel. The
// queue journals an event before bus delivery, so restart can replay a missed
// delivery without inferring a result from a disappeared process.

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

type weaveOwnerNotice struct {
	ID        int64         `json:"id"`
	Key       string        `json:"key"`
	Owner     string        `json:"owner"`
	Activity  room.Activity `json:"activity"`
	Delivered bool          `json:"delivered,omitempty"`
	// Legacy fields are retained solely to replay notices written before the
	// activity envelope existed. They are never re-published verbatim.
	Type          string    `json:"type,omitempty"`
	Source        string    `json:"source,omitempty"`
	Repo          string    `json:"repo,omitempty"`
	Run           int64     `json:"run,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	Timestamp     time.Time `json:"timestamp,omitempty"`
	TerminalState string    `json:"terminal_state,omitempty"`
	Evidence      string    `json:"evidence,omitempty"`
}

// weaveOwnerFor resolves only durable ownership. It never picks an arbitrary
// active worker merely because that worker happens to be reachable.
func weaveOwnerFor(dir string, q *weaveQueue, it *weaveItem) string {
	if l, ok := loadConductorLock(dir); ok && strings.TrimSpace(l.Holder) != "" {
		return strings.TrimSpace(l.Holder)
	}
	if q != nil && it != nil {
		for _, s := range q.Stories {
			for _, r := range s.Runs {
				if r.ID == it.ID && s.Lease != nil && strings.TrimSpace(s.Lease.Holder) != "" {
					return strings.TrimSpace(s.Lease.Holder)
				}
			}
		}
	}
	if it != nil {
		return strings.TrimSpace(it.Owner)
	}
	return ""
}

func weaveQueueOwnerNotice(dir string, q *weaveQueue, it *weaveItem, typ string) {
	if q == nil || it == nil || strings.TrimSpace(typ) == "" {
		return
	}
	owner := weaveOwnerFor(dir, q, it)
	if owner == "" {
		return
	} // no explicit accountable owner: do not guess.
	key := fmt.Sprintf("weave:%s:%d:%s:%s", q.Root, it.ID, typ, it.State)
	for _, e := range q.OwnerNotices {
		if e.Key == key {
			return
		}
	}
	q.NextOwnerNoticeID++
	verb := "updated"
	if typ == "assignment-started" {
		verb = "assigned"
	}
	if typ == "run-terminal" {
		verb = "completed"
	}
	now := time.Now().UTC()
	actor := strings.TrimSpace(it.Owner)
	if actor == "" {
		actor = "weave"
	}
	q.OwnerNotices = append(q.OwnerNotices, weaveOwnerNotice{ID: q.NextOwnerNoticeID, Key: key, Owner: owner, Activity: room.Activity{
		ID: key, Version: 1, Actor: actor, Verb: verb, Noun: "run",
		ObjectRef: fmt.Sprintf("run:%d", it.ID), Repo: q.Root, Origin: "weave",
		CorrelationID: fmt.Sprintf("weave-run-%d", it.ID), Priority: bus.DeliveryQueued,
		Timestamp: now, FetchRef: fmt.Sprintf("weave:run:%d", it.ID),
		Summary: fmt.Sprintf("weave run #%d %s", it.ID, it.State),
	}})
}

// weaveDeliverOwnerNotices runs after the queue write. It is safe on every
// recovery path: a stable key is checked in the durable timeline before
// publishing, covering a crash after publish but before its delivery receipt.
func weaveDeliverOwnerNotices(dir string) {
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return
	}
	for _, e := range q.OwnerNotices {
		if e.Delivered || e.Owner == "" {
			continue
		}
		_, _ = bus.EnsureSubscription(e.Owner) // offline owner gets a durable inbox.
		activity := weaveNoticeActivity(e)
		subject := activity.Summary
		if !weaveOwnerNoticeOnTimeline(e.Key) {
			if err := bus.Publish(bus.Notification{Principal: activity.Actor, To: e.Owner, Body: subject, Priority: bus.DeliveryQueued, Activity: &activity, MatchReason: "owner"}); err != nil {
				continue
			}
			// A live addressed conversation is woken; a watcher cursor alone is
			// not delivery and does not consume this durable event.
			_ = bus.SteerLive(e.Owner, subject)
		}
		_ = withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
			for i := range fresh.OwnerNotices {
				if fresh.OwnerNotices[i].ID == e.ID {
					fresh.OwnerNotices[i].Delivered = true
					break
				}
			}
			return nil
		})
	}
}

// weaveNoticeActivity migrates an undelivered v1 owner notice at the replay
// boundary. Its old evidence field is intentionally not copied: details stay
// behind the authorized fetch reference.
func weaveNoticeActivity(e weaveOwnerNotice) room.Activity {
	if e.Activity.Validate() == nil {
		return e.Activity
	}
	verb := "updated"
	if e.Type == "assignment-started" {
		verb = "assigned"
	}
	if e.Type == "run-terminal" {
		verb = "completed"
	}
	actor := strings.TrimSpace(e.Actor)
	if actor == "" {
		actor = "weave"
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return room.Activity{ID: e.Key, Version: 1, Actor: actor, Verb: verb, Noun: "run",
		ObjectRef: fmt.Sprintf("run:%d", e.Run), Repo: e.Repo, Origin: "weave",
		CorrelationID: fmt.Sprintf("weave-run-%d", e.Run), Priority: bus.DeliveryQueued,
		Timestamp: ts, FetchRef: fmt.Sprintf("weave:run:%d", e.Run),
		Summary: fmt.Sprintf("weave run #%d %s", e.Run, e.TerminalState)}
}

func weaveOwnerNoticeOnTimeline(key string) bool {
	events, err := room.Timeline(0)
	if err != nil {
		return false
	}
	for _, e := range events {
		if e.Activity != nil && e.Activity.ID == key {
			return true
		}
	}
	return false
}
