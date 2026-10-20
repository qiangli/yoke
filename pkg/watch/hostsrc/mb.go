package hostsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/watch"
)

// MBSource watches the host message board.
//
// It reads the board's own API — the public log plus the per-reader cursor — and
// adds no store. The board is append-only and pull-read, which is exactly the
// shape a poll source wants: a watcher that was not running when a post landed
// still sees it on the next read.
type MBSource struct {
	BoardAPI Board
	// Backfill reports posts already on the board that are younger than this.
	// Zero starts at the head: a watcher must not open by replaying history,
	// which is how the hand-written script announced a whole night at once.
	Backfill time.Duration
	// Recipient narrows the source to posts addressed to one agent or seat.
	Recipient string
	// MaxTracked bounds the receipt table. A watcher is a long-running process;
	// an unbounded map keyed by every post it ever saw is a leak with a
	// schedule.
	MaxTracked int

	started bool
	lastSeq int64
	tracked []trackedPost
	read    map[string]bool
}

type trackedPost struct {
	post bus.Post
	conv string
}

// NewMBSource builds the board source.
func NewMBSource(b Board) *MBSource {
	if b == nil {
		b = LiveBoard{}
	}
	return &MBSource{BoardAPI: b, MaxTracked: 256, read: map[string]bool{}}
}

// Name implements watch.Source.
func (s *MBSource) Name() string { return "mb" }

// BackfillWindow implements watch.Backfiller.
func (s *MBSource) BackfillWindow() time.Duration { return s.Backfill }

// Poll implements watch.Source.
func (s *MBSource) Poll(_ context.Context, now time.Time) ([]watch.Event, error) {
	posts, err := s.BoardAPI.Posts()
	if err != nil {
		return nil, fmt.Errorf("board unreadable: %w", err)
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].Seq < posts[j].Seq })

	var out []watch.Event
	for _, p := range posts {
		if p.Seq <= s.lastSeq {
			continue
		}
		s.lastSeq = p.Seq
		if s.Recipient != "" && !strings.EqualFold(strings.TrimSpace(p.To), s.Recipient) {
			continue
		}
		at := postTime(p, now)
		if !s.started && now.Sub(at) > s.Backfill {
			continue // before our window: baseline, not news
		}
		ev := s.event(p, at)
		s.track(p, ev.Attr("conv"))
		out = append(out, ev)
	}
	s.started = true
	out = append(out, s.receipts(now)...)
	return out, nil
}

// event renders one post as an observation.
func (s *MBSource) event(p bus.Post, at time.Time) watch.Event {
	id := p.ID
	if id == "" {
		id = "seq-" + strconv.FormatInt(p.Seq, 10)
	}
	raw, _ := json.Marshal(p)
	attrs := map[string]string{
		"id":    id,
		"seq":   strconv.FormatInt(p.Seq, 10),
		"from":  p.From,
		"to":    strings.TrimSpace(p.To),
		"topic": p.Topic,
		"conv":  conversation(p),
	}
	if attrs["to"] != "" {
		attrs["directed"] = "true"
		// to_role is the readable seat name, so a rule can say "operator"
		// without knowing the machine address a seat routes on.
		attrs["to_role"] = roleLabel(attrs["to"])
	}
	return watch.Event{
		At: at, Source: "mb", Kind: watch.KindAdded, Locator: "mb:" + id, Key: id,
		Text: watch.SanitizeText([]byte(firstLine(p.Body, 200))), Attrs: attrs, Raw: raw,
	}
}

// receipts reports the READ rung: the recipient's own cursor has moved past the
// post. It is a separate observation from the post itself because the
// difference between "it is on the board" and "they have looked at it" is the
// difference between a queued message and an acknowledged one.
func (s *MBSource) receipts(now time.Time) []watch.Event {
	var out []watch.Event
	for _, t := range s.tracked {
		id := t.post.ID
		if id == "" {
			id = "seq-" + strconv.FormatInt(t.post.Seq, 10)
		}
		if s.read[id] {
			continue
		}
		reader := strings.TrimSpace(t.post.To)
		if reader == "" {
			continue
		}
		if s.BoardAPI.SeenSeq(reader) < t.post.Seq {
			continue
		}
		s.read[id] = true
		out = append(out, watch.Event{
			At: now, Source: "mb", Kind: watch.KindChanged, Locator: "mb:" + id, Key: id,
			Field: "read", Before: "queued", After: "read",
			Text: fmt.Sprintf("%s read mb:%s", reader, id),
			Attrs: map[string]string{
				"post": id, "to": reader, "from": t.post.From,
				"conv": t.conv, "to_role": roleLabel(reader),
			},
		})
	}
	return out
}

func (s *MBSource) track(p bus.Post, conv string) {
	if strings.TrimSpace(p.To) == "" {
		return
	}
	s.tracked = append(s.tracked, trackedPost{post: p, conv: conv})
	if s.MaxTracked > 0 && len(s.tracked) > s.MaxTracked {
		drop := s.tracked[:len(s.tracked)-s.MaxTracked]
		for _, t := range drop {
			delete(s.read, t.post.ID)
		}
		s.tracked = append([]trackedPost(nil), s.tracked[len(s.tracked)-s.MaxTracked:]...)
	}
}

// MBReplyLevels is the board's acknowledgment ladder.
//
// delivered -> read -> replied, and only `replied` answers. The board has no
// reply-to field, so `replied` is "a post back the other way in the same
// conversation" — see conversation() for what that means and why.
func MBReplyLevels() []watch.AckLevel {
	return []watch.AckLevel{
		{Name: "delivered", Match: func(ev, trig watch.Event) bool {
			return ev.Kind == watch.KindAdded && ev.Attr("id") == trig.Attr("id")
		}},
		{Name: "read", Match: func(ev, trig watch.Event) bool {
			return ev.Field == "read" && ev.Attr("post") == trig.Attr("id")
		}},
		{Name: "replied", Match: func(ev, trig watch.Event) bool {
			return ev.Kind == watch.KindAdded &&
				ev.Attr("from") != "" && trig.Attr("to") != "" &&
				strings.EqualFold(ev.Attr("from"), trig.Attr("to")) &&
				strings.EqualFold(ev.Attr("to"), trig.Attr("from"))
		}},
	}
}

// conversation is the correlation key for a post.
//
// A topic IS the thread, so two agents trading messages under one topic are one
// conversation. Without a topic the best available join is the PAIR — the board
// carries no reply-to id — so the next post back the other way counts as the
// answer. That is a heuristic and it is the honest one: the alternative is to
// report every untopiced message as unanswered forever, which trains a reader
// to ignore the alert.
func conversation(p bus.Post) string {
	if t := strings.TrimSpace(p.Topic); t != "" {
		return "topic:" + t
	}
	from, to := strings.TrimSpace(p.From), strings.TrimSpace(p.To)
	if from == "" || to == "" {
		return ""
	}
	a, b := strings.ToLower(from), strings.ToLower(to)
	if a > b {
		a, b = b, a
	}
	return "pair:" + a + "|" + b
}

func roleLabel(addr string) string {
	label := bus.RoleLabelFor(addr)
	if label == "" {
		return addr
	}
	return label
}

func postTime(p bus.Post, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(p.At)); err == nil {
		return t
	}
	return fallback
}

func firstLine(body string, max int) string {
	body = strings.TrimSpace(body)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	if len(body) > max {
		return body[:max] + "..."
	}
	return body
}
