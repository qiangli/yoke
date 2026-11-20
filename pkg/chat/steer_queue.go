package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

// SteerInboxPrefix advertises the run's durable board address on its room card.
const SteerInboxPrefix = "steer-inbox:"

// newSteerInboxCap names a fresh steer inbox for one chat session and opens
// its board cursor at the current head before the card is published. Nothing
// can address the inbox earlier, so no steer is skipped; without the cursor the
// relay's first read is a full-board audience scan (tens of seconds on a
// long-lived host board), which delayed the first queued steer by minutes.
// A failed initialization only costs that slow first read.
func newSteerInboxCap() string {
	addr := "chat-steer-" + bus.NewPostID()
	_ = bus.InitializeReaderAtHead(addr)
	return SteerInboxPrefix + addr
}

func steerInbox(c room.Card) string {
	for _, cap := range c.Caps {
		if addr, ok := strings.CutPrefix(cap, SteerInboxPrefix); ok {
			return addr
		}
	}
	return ""
}

// SteerRelay observes the worker screen and drains only the lines addressed to
// this session. Close waits for its last send before the socket can be reused.
type SteerRelay struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	tail   string
	ready  bool
}

var promptRow = regexp.MustCompile(`[\r\n]|\x1b\[[0-9]*;[0-9]*[Hf]`)

func (p *SteerRelay) GateAnswered(agentpty.GateVerdict, string) {
	p.mu.Lock()
	p.tail = ""
	p.mu.Unlock()
}

func (p *SteerRelay) Close() {
	p.cancel()
	<-p.done
}

func (p *SteerRelay) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return len(b), nil
	}
	p.tail += string(b)
	if len(p.tail) > readyTailBytes {
		p.tail = p.tail[len(p.tail)-readyTailBytes:]
	}
	if strings.ContainsAny(p.tail, "❯›>") && agentpty.ClassifyGate(p.tail).Kind == agentpty.GateNone {
		for _, line := range promptRow.Split(p.tail, -1) {
			fields := strings.Fields(agentpty.ScreenText(line))
			if len(fields) > 0 && (fields[0] == "❯" || fields[0] == "›" || (len(fields) == 1 && fields[0] == ">")) {
				p.ready = true
				p.tail = ""
				break
			}
		}
	}
	return len(b), nil
}

func (p *SteerRelay) seen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready
}

// StartSteerRelay watches raw PTY output for the first input prompt. A
// launcher may also declare the exact session ready on the room timeline.
func StartSteerRelay(c room.Card, replayedThrough int64, openingDelivered func() bool) *SteerRelay {
	return startSteerRelay(c, replayedThrough, openingDelivered, managedInboxPoll, defaultInteractiveDeliveryTiming.readyTimeout, os.Stderr)
}

func startSteerRelay(c room.Card, replayedThrough int64, openingDelivered func() bool, poll, readyTimeout time.Duration, warnings io.Writer) *SteerRelay {
	ctx, cancel := context.WithCancel(context.Background())
	prompt := &SteerRelay{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(prompt.done)
		status := room.NewTimelineReader(func(e room.Event) bool {
			return e.Type == room.EventStatus && e.Target == c.ID && strings.EqualFold(strings.TrimSpace(e.Body), "ready") && e.TS >= c.Joined
		})
		addr := steerInbox(c)
		gate := bus.NewInboxPollGate(addr)
		tick := time.NewTicker(poll)
		defer tick.Stop()
		deadline := time.Now().Add(readyTimeout)
		ready := false
		inputReady := func() bool {
			if !ready && !prompt.seen() {
				pos, err := status.Latest(ctx)
				ready = err == nil && pos.Seq > 0
				if !ready && !time.Now().Before(deadline) {
					fmt.Fprintf(warnings, "chat: warning: %s did not signal readiness within %s; delivering queued steers anyway\n", c.ID, readyTimeout)
					ready = true
				}
			} else {
				ready = true
			}
			return ready
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if openingDelivered != nil && !openingDelivered() {
				continue
			}
			read, sum, ok := gate.Due(time.Now())
			if !read {
				continue
			}
			posts, _, _, err := bus.Unseen(addr, 1)
			if err != nil {
				continue
			}
			for _, post := range posts {
				// The run mailbox survives resume. Its opening prompt may
				// already contain the earlier part of the queue.
				if post.Seq <= replayedThrough {
					continue
				}
				if !inputReady() {
					break
				}
				if err = deliverSteer(c, post.Body); err != nil {
					break
				}
				if err = bus.MarkSeen(addr, post.Seq); err != nil {
					break
				}
				_ = room.Emit(room.Event{Type: room.EventSteer, Actor: post.From, Target: c.ID, Body: post.Body})
			}
			if err == nil && (len(posts) == 0 || ready) {
				gate.Commit(sum, ok, time.Now())
			} else {
				gate.Retry()
			}
		}
	}()
	return prompt
}
