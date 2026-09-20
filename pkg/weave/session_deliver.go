package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/meet"
)

// Local delivery — the MDA half of the email model.
//
// `bashy inbox` drains the repo session's feed and files every message
// addressed to the reader into the reader's OWN board, under the message's
// universal id (PostMessageOnce, key = id). From then on the existing inbox
// path — Unseen, the directed/capped rules, the mb cursor, --watch — renders
// it exactly as local mail. Nothing new is read; a second delivery of the
// same id is a no-op; the feed cursor is advanced only after the batch was
// filed, so a crash mid-batch re-delivers idempotently rather than losing
// mail.
//
// Delivery is not reading: it runs on every inbox pass, --peek included, and
// never touches a read cursor.

// DeliveryReport says what one pass did, and WHEN the feed answered — a
// shared view that does not say how fresh it is reads as truth.
type DeliveryReport struct {
	Session   string    `json:"session,omitempty"`
	Delivered int       `json:"delivered"`
	Cursor    string    `json:"cursor,omitempty"`
	AsOf      time.Time `json:"as_of"`
	// Skipped is set when there is no relay to drain (not paired, no origin):
	// an unpaired host reads its inbox exactly as before, silently.
	Skipped string `json:"skipped,omitempty"`
}

var feedCursorName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func feedCursorPath(repoRoot, taskID, reader string) (string, error) {
	dir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return "", err
	}
	name := "feed-cursor-" + feedCursorName.ReplaceAllString(taskID, "-") + "-" + feedCursorName.ReplaceAllString(reader, "-")
	return filepath.Join(dir, name), nil
}

func readFeedCursor(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeFeedCursor(path, cursor string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := weaveWriteFile(tmp, []byte(cursor+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// addressedTo reports whether an envelope is the reader's mail. A message
// names a seat (`<name>@<host>`) or a role; the role rungs are late-bound to
// the session's lease holder at READ time, so a handover re-targets in-flight
// mail instead of orphaning it (docs/agent-inbox-unified-delivery.md §3).
func addressedTo(to, reader, me string, holder *string) bool {
	to = strings.TrimSpace(to)
	iAmHolder := holder != nil && strings.EqualFold(strings.TrimSpace(*holder), me)
	switch {
	case to == "":
		return iAmHolder
	case strings.EqualFold(to, me), strings.EqualFold(to, reader):
		return true
	case strings.EqualFold(to, readerSeat(reader, me)):
		// `<reader>@<this host>`: the reader's own seat address. The process
		// running the pass may be another identity on the host (a live
		// session's wrapper relays for its agent under the person's
		// identity), so `me` alone misses mail addressed to the seat — and
		// the pass then advanced the cursor past it, losing the message
		// (sprint 220, story ad5b0de9).
		return true
	case strings.EqualFold(to, "conductor"), strings.HasPrefix(strings.ToLower(to), "conductor:"), strings.EqualFold(to, "owner"):
		return iAmHolder
	}
	return false
}

// readerSeat is the reader's address on this host: `<reader>@<host>`, with
// the host taken from the process's own signature. Empty when the reader is
// already an address or the host is unknown.
func readerSeat(reader, me string) string {
	if strings.Contains(reader, "@") {
		return reader
	}
	i := strings.LastIndexByte(me, '@')
	if i < 0 || reader == "" {
		return ""
	}
	return reader + me[i:]
}

// DeliverRemoteMail runs one delivery pass for reader in the checkout at
// repoRoot. An unpaired host, or one with no origin, is not an error: the
// report says it was skipped and why, and local mail is unaffected.
func DeliverRemoteMail(ctx context.Context, repoRoot, reader string) (DeliveryReport, error) {
	rep := DeliveryReport{AsOf: time.Now().UTC()}
	sc, err := EnsureRepoSession(ctx, repoRoot)
	if err != nil {
		if errors.Is(err, ErrNotPaired) || errors.Is(err, ErrNoOrigin) {
			rep.Skipped = err.Error()
			return rep, nil
		}
		return rep, err
	}
	taskID := sc.pointer.TaskID
	rep.Session = taskID
	cpath, err := feedCursorPath(repoRoot, taskID, reader)
	if err != nil {
		return rep, err
	}
	cursor := readFeedCursor(cpath)
	me, _ := SessionParticipant()

	// The lease holder is needed only for the late-bound role rungs, and
	// only when a message actually arrives — one extra call per delivery,
	// none per idle poll.
	var holder *string
	holderKnown := false
	lookupHolder := func() *string {
		if holderKnown {
			return holder
		}
		holderKnown = true
		if tasks, terr := sc.client.ListTasks(ctx); terr == nil {
			for _, t := range tasks {
				if t.ID == taskID {
					holder = t.LeaseHolder
					break
				}
			}
		}
		return holder
	}

	for {
		resp, err := sc.client.GetEvents(ctx, taskID, cursor, 100)
		if err != nil {
			return rep, fmt.Errorf("session feed: %w", err)
		}
		rep.AsOf = time.Now().UTC()
		for _, ev := range resp.Events {
			if ev.Kind != MessageEventKind {
				continue
			}
			var d messageDetail
			if json.Unmarshal(ev.Detail, &d) != nil || d.ID == "" {
				continue
			}
			if strings.EqualFold(d.From, me) {
				continue // my own post is already on my board / in my room
			}
			if d.Room != "" {
				// A post in a shared meet room: file it into this host's
				// mirror of the room (created on first sight), for everyone
				// seated there — not into one reader's mailbox.
				at, _ := time.Parse(time.RFC3339Nano, d.At)
				filed, err := meet.DeliverShared(meet.SharedEvent{
					ID: d.ID, RoomID: d.Room, Topic: d.RoomTopic, Session: taskID, Roster: d.Roster,
					From: d.From, To: d.To, Kind: d.Kind, Body: ev.Summary, At: at,
				}, sessionHostName())
				if err != nil {
					return rep, fmt.Errorf("deliver room post %s: %w", d.ID, err)
				}
				if filed {
					rep.Delivered++
				}
				continue
			}
			if !addressedTo(d.To, reader, me, lookupHolder()) {
				continue
			}
			// Keyed by message AND reader: one host may legitimately file the
			// same message for two readers (a broadcast, a role address
			// re-targeted at read time, a second seat reading the feed), and
			// a key of the id alone made the second filing fail as "notice
			// key reused with different content" — which then aborted the
			// whole pass, so nothing after it was delivered either.
			if _, err := bus.PostMessageOnce(ctx, d.ID+"|"+reader, bus.Post{ID: d.ID, From: d.From, To: reader, Topic: d.Topic, Body: ev.Summary}); err != nil {
				return rep, fmt.Errorf("deliver %s: %w", d.ID, err)
			}
			rep.Delivered++
		}
		next := resp.Cursor
		if next == "" && len(resp.Events) > 0 {
			next = resp.Events[len(resp.Events)-1].ID
		}
		if next != "" && next != cursor {
			if err := writeFeedCursor(cpath, next); err != nil {
				return rep, err
			}
			cursor = next
		}
		rep.Cursor = cursor
		if len(resp.Events) < 100 {
			return rep, nil
		}
	}
}
