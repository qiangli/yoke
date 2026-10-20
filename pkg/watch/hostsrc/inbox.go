package hostsrc

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/watch"
)

// InboxSink posts alerts to the host message board.
//
// This is the sink that makes a watcher useful to somebody who is not watching:
// the party who needs to know a sprint stalled is usually mid-turn in another
// session, and the board is durable and pull-read, so a post to an agent that
// is not running is there when it next looks.
//
// Engine policy applies BEFORE delivery — transitions only, dedupe, re-notify —
// which is what keeps this from becoming a megaphone. The board deletes
// nothing, so a sink that posted every cycle would be permanent noise.
type InboxSink struct {
	BoardAPI Board
	// From is the sender. The board rejects an unattributable post, and rightly:
	// nobody can ask an anonymous sender what it meant.
	From string
	// To is the recipients — an agent name or a seat. One post each.
	To []string
	// Topic groups the posts on the board.
	Topic string
}

// NewInboxSink builds the sink over the live board.
func NewInboxSink(from string, to []string, topic string) *InboxSink {
	if topic == "" {
		topic = "watch"
	}
	return &InboxSink{BoardAPI: LiveBoard{}, From: from, To: to, Topic: topic}
}

// Deliver implements watch.Sink.
func (s *InboxSink) Deliver(a watch.Alert) error {
	if strings.TrimSpace(s.From) == "" {
		return fmt.Errorf("watch: the inbox sink needs a sender")
	}
	if len(s.To) == 0 {
		return fmt.Errorf("watch: the inbox sink needs a recipient")
	}
	board := s.BoardAPI
	if board == nil {
		board = LiveBoard{}
	}
	body := InboxBody(a)
	var firstErr error
	for _, to := range s.To {
		to = strings.TrimSpace(to)
		if to == "" {
			continue
		}
		err := board.Post(bus.Post{From: s.From, To: to, Topic: s.Topic, Body: body})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// InboxBody renders the post. It names the rule, the state and the locator on
// the first line, because a reader scanning a board decides from that line
// whether this is theirs to act on.
func InboxBody(a watch.Alert) string {
	var sb strings.Builder
	verb := "ALERT"
	if a.State == watch.StateClear {
		verb = "CLEAR"
	}
	fmt.Fprintf(&sb, "watch %s: %s %s", verb, a.Rule, a.Reason)
	if a.Locator != "" {
		fmt.Fprintf(&sb, " on %s", watch.SanitizeText([]byte(a.Locator)))
	}
	sb.WriteByte('\n')
	if a.Message != "" {
		fmt.Fprintf(&sb, "%s\n", watch.SanitizeText([]byte(a.Message)))
	}
	if !a.At.IsZero() {
		fmt.Fprintf(&sb, "at %s\n", a.At.UTC().Format("2006-01-02 15:04:05Z"))
	}
	return sb.String()
}
