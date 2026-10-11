package room

import (
	"context"
	"time"
)

// EventResync is the synthetic type of a Resync item. It is never written to
// the timeline.
const EventResync = "resync"

const (
	ResyncRetention = "retention" // the requested position precedes archivedThrough
	ResyncRebase    = "rebase"    // the stream was replaced: generation changed, or the position is past the head
)

// Resync tells a consumer its position can no longer be continued gaplessly.
// Seq is the position the stream continues from: every event after Seq is
// delivered next, so a consumer that rebases to it loses nothing further.
type Resync struct {
	Reason          string `json:"reason"`
	Since           int64  `json:"since"`
	ArchivedThrough int64  `json:"archived_through"`
	Seq             int64  `json:"seq"`
}

// StreamItem is one delivery: an Event, or (Resync != nil) a rebase notice.
type StreamItem struct {
	Event  Event
	Resync *Resync
}

// StreamOptions selects what Stream delivers.
type StreamOptions struct {
	// After delivers events with Seq > After. Explicit marks After as a request
	// (even After == 0); otherwise follow starts at the current head and a
	// one-shot read starts at the oldest live event.
	After    int64
	Explicit bool
	Follow   bool
	Poll     time.Duration
	Match    func(Event) bool
}

// Stream is THE reader of the host event log: the `events` verb and the SSE
// endpoint both run it, so they cannot disagree about gaps. It reads the room
// timeline (global monotonic Seq, stable across rotation) and reports, rather
// than hides, the two ways a position can be lost: retention archived events
// the caller had not read, or the stream was replaced under it. Either yields a
// Resync item, then delivery continues from Resync.Seq.
func Stream(ctx context.Context, opt StreamOptions, fn func(StreamItem) error) error {
	poll := opt.Poll
	if poll <= 0 {
		poll = time.Second
	}
	reader := NewTimelineReader(nil)
	pos, err := reader.Latest(ctx)
	if err != nil {
		return err
	}
	gen := pos.Generation
	after := opt.After
	started := opt.Explicit
	lastHead := int64(-1)
	rebase := false

	var ticker *time.Ticker
	for {
		if lastHead >= 0 {
			pos, err = reader.Latest(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if pos.Generation != gen {
				gen = pos.Generation
				rebase = true
			}
		}
		if lastHead < 0 || rebase || pos.Seq != lastHead {
			events, err := Timeline(0)
			if err != nil {
				return err
			}
			head := ArchivedThrough()
			if len(events) > 0 {
				head = events[len(events)-1].Seq
			}
			if !started {
				started = true
				if opt.Follow {
					after = head
				} else {
					after = ArchivedThrough()
				}
			}
			if rebase {
				rebase = false
				if err := fn(StreamItem{Resync: &Resync{Reason: ResyncRebase, Since: after, ArchivedThrough: ArchivedThrough(), Seq: head}}); err != nil {
					return err
				}
				after = head
			}
			if after > head {
				if err := fn(StreamItem{Resync: &Resync{Reason: ResyncRebase, Since: after, ArchivedThrough: ArchivedThrough(), Seq: head}}); err != nil {
					return err
				}
				after = head
			}
			floor := ArchivedThrough()
			if len(events) > 0 {
				floor = events[0].Seq - 1
			}
			if after < floor {
				if err := fn(StreamItem{Resync: &Resync{Reason: ResyncRetention, Since: after, ArchivedThrough: ArchivedThrough(), Seq: floor}}); err != nil {
					return err
				}
				after = floor
			}
			for _, e := range events {
				if e.Seq <= after {
					continue
				}
				after = e.Seq
				if opt.Match == nil || opt.Match(e) {
					if err := fn(StreamItem{Event: e}); err != nil {
						return err
					}
				}
			}
			lastHead = pos.Seq
		}
		if !opt.Follow {
			return nil
		}
		if ticker == nil {
			ticker = time.NewTicker(poll)
			defer ticker.Stop()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
