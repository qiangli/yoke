// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"time"
)

// Follow streams journal entries as they are appended, calling fn for each new
// one, until ctx is cancelled.
//
// Polling, not inotify, and deliberately so: the journal is append-only and
// hash-chained, so "what is new" is answerable by replaying and skipping
// everything at or below a watermark. That makes follow a pure function of the
// journal — the same property every other view has — instead of a second,
// event-driven code path that could disagree with a plain `steward log`.
//
// It is also what makes it TESTABLE. A test drives it with a cancellable context
// and a short interval, appends entries, and asserts exactly what arrived; there
// is no filesystem-notification race to sleep around. A follow implementation you
// cannot test is one you cannot trust to have delivered everything, and a log
// tailer that silently drops an entry is worse than no tailer at all.
//
// A corrupt tail does not stop the follow: entries before the damage are still
// delivered, and the stream simply does not advance past it — the same
// "valid prefix" rule the rest of the package lives by. fn returning an error
// stops the follow and that error is returned.
func (s *Store) Follow(ctx context.Context, f Filter, interval time.Duration, fn func(Entry) error) error {
	if interval <= 0 {
		interval = time.Second
	}

	// Start from the current head so a follower sees what happens NEXT. Callers
	// wanting the backlog print it with Log first — mixing the two here would make
	// "did I already see this?" the caller's problem.
	rep, err := s.Replay()
	if err != nil {
		return err
	}
	seen := rep.HeadSeq

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A cancelled follow is how follow ENDS, not how it fails. Ctrl-C is the
			// expected exit, so returning ctx.Err() here would make every normal use
			// look like an error to a shell that checks $?.
			return nil
		case <-t.C:
			rep, err := s.Replay()
			if err != nil {
				return err
			}
			for _, e := range rep.Entries {
				if e.Seq <= seen {
					continue
				}
				seen = e.Seq
				if !f.Match(e) {
					continue
				}
				if err := fn(e); err != nil {
					return err
				}
			}
		}
	}
}
