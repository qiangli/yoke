package bus

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/admission"
)

// largeBacklogHost models the host unified-inbox adapter behind the reported
// flood: ~781 board posts for a fresh binding, each with its own retrieval
// reference (so the overflow manifest cannot collapse them into one group),
// a few addressed to the binding itself. It keeps every item durably (NEVER
// DROP) and only withholds unacknowledged ones from the next snapshot
// (DEMOTE): acknowledgement is the watermark.
type largeBacklogHost struct {
	mu   sync.Mutex
	all  []admission.Item
	seen map[string]bool
}

func newLargeBacklogHost(agent string, total, addressed int) *largeBacklogHost {
	h := &largeBacklogHost{seen: map[string]bool{}}
	for i := 0; i < total; i++ {
		to := ""
		if i < addressed {
			to = agent
		}
		id := fmt.Sprintf("mb:%d", 10000+i)
		body := fmt.Sprintf("board post %d for the fleet", i)
		h.all = append(h.all, admission.Item{
			Source: "mb", ID: id, Sequence: int64(10000 + i),
			From: "operator", To: to, Topic: "mb",
			Priority:    admission.PriorityInformational,
			Body:        body,
			ArtifactRef: fmt.Sprintf("bashy inbox --id %s --peek", id),
			OverflowRef: fmt.Sprintf("bashy inbox --id %s --peek", id),
		})
	}
	for i := range h.all {
		id := h.all[i].ID
		h.all[i].Acknowledge = func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.seen[id] = true
			return nil
		}
	}
	return h
}

func (h *largeBacklogHost) items() []admission.Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]admission.Item, 0, len(h.all))
	for _, it := range h.all {
		if !h.seen[it.ID] {
			out = append(out, it)
		}
	}
	return out
}

func (h *largeBacklogHost) unacked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.all) - len(h.seen)
}

// A fresh binding with a large board backlog: the first turn must stay
// bounded no matter how large the backlog is.
func TestPrependFreshBindingLargeBacklogStaysBounded(t *testing.T) {
	isolate(t)
	const total = 781
	host := newLargeBacklogHost("fresh-binding", total, 3)
	old := PrepareTurnItems
	PrepareTurnItems = func(agent string) ([]admission.Item, error) { return host.items(), nil }
	t.Cleanup(func() { PrepareTurnItems = old })

	p := PrepareForAgent("fresh-binding", "do the task")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	if len(p.Text) > 4096 {
		t.Fatalf("first-turn prepend is %d bytes for %d backlog items, want a bounded block", len(p.Text), total)
	}
	if !strings.Contains(p.Text, "1 of 781 unread (3 addressed to you)") {
		t.Fatalf("prepend hides the totals, got:\n%s", p.Text)
	}
	if !strings.Contains(p.Text, `bashy inbox --as "fresh-binding"`) {
		t.Fatalf("prepend names no retrieval path, got:\n%s", p.Text)
	}
	if rep := p.AdmissionReport(); rep.AdmittedItems != 1 || rep.InputItems != total {
		t.Fatalf("want exactly 1 admitted of 781 input items, got %+v", rep)
	}
	// Delivered: the shown overflow advances the watermark...
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	// ...so the next turn is quiet...
	again := PrepareForAgent("fresh-binding", "next task")
	if err := again.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again.Text, "board post") {
		t.Fatalf("committed backlog was re-sent on the next turn:\n%.500s", again.Text)
	}
	// ...while every record stays durable (DEMOTE, NEVER DROP).
	if host.unacked() != 0 {
		t.Fatalf("%d backlog items still unread after a delivered overflow", host.unacked())
	}
	if len(host.all) != total {
		t.Fatalf("backlog store lost records: %d of %d", len(host.all), total)
	}
}

// The cap spends in order: addressed to this agent first, then highest
// priority, then newest.
func TestPrependCapOrdersAddressedPriorityNewest(t *testing.T) {
	isolate(t)
	old := PrepareTurnItems
	PrepareTurnItems = func(agent string) ([]admission.Item, error) {
		mk := func(id, to string, pri admission.Priority, seq int64, body string) admission.Item {
			return admission.Item{Source: "mb", ID: id, Sequence: seq, To: to,
				Priority: pri, Body: body,
				ArtifactRef: "bashy inbox --id " + id + " --peek",
				OverflowRef: "bashy inbox --peek",
				Acknowledge: func() error { return nil }}
		}
		return []admission.Item{
			mk("mb:1", "", admission.PriorityInformational, 1, "old broadcast"),
			mk("mb:2", "", admission.PriorityDirected, 2, "newer directed broadcast"),
			mk("mb:3", "agent-a", admission.PriorityInformational, 3, "dm for agent-a"),
		}, nil
	}
	t.Cleanup(func() { PrepareTurnItems = old })

	p := PrepareForAgent("agent-a", "go")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "dm for agent-a") {
		t.Fatalf("addressed mail must win the single slot, got:\n%s", p.Text)
	}
	if strings.Contains(p.Text, "old broadcast") || strings.Contains(p.Text, "newer directed") {
		t.Fatalf("cap withheld items leaked inline, got:\n%s", p.Text)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	// Next turn the addressed item is consumed, so priority wins over newness.
	PrepareTurnItems = func(agent string) ([]admission.Item, error) {
		mk := func(id string, pri admission.Priority, seq int64, body string) admission.Item {
			return admission.Item{Source: "mb", ID: id, Sequence: seq,
				Priority: pri, Body: body,
				ArtifactRef: "bashy inbox --id " + id + " --peek",
				OverflowRef: "bashy inbox --peek",
				Acknowledge: func() error { return nil }}
		}
		return []admission.Item{
			mk("mb:1", admission.PriorityInformational, 1, "old broadcast"),
			mk("mb:2", admission.PriorityDirected, 2, "newer directed broadcast"),
		}, nil
	}
	q := PrepareForAgent("agent-a", "go")
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.Text, "newer directed broadcast") {
		t.Fatalf("priority must beat newness, got:\n%s", q.Text)
	}
}

// BASHY_CHAT_INBOX_MAX raises the inline count; garbage falls back to one.
func TestPrependCapHonorsChatInboxMax(t *testing.T) {
	isolate(t)
	old := PrepareTurnItems
	PrepareTurnItems = func(agent string) ([]admission.Item, error) {
		var out []admission.Item
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("mb:%d", i)
			out = append(out, admission.Item{Source: "mb", ID: id, Sequence: int64(i),
				Body:        fmt.Sprintf("post %d", i),
				ArtifactRef: "bashy inbox --id " + id + " --peek",
				OverflowRef: "bashy inbox --peek",
				Acknowledge: func() error { return nil }})
		}
		return out, nil
	}
	t.Cleanup(func() { PrepareTurnItems = old })

	t.Setenv("BASHY_CHAT_INBOX_MAX", "3")
	p := PrepareForAgent("agent-a", "go")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "3 of 5 unread (0 addressed to you)") {
		t.Fatalf("MAX=3 must show three plus a counter, got:\n%s", p.Text)
	}

	t.Setenv("BASHY_CHAT_INBOX_MAX", "bogus")
	q := PrepareForAgent("agent-a", "go")
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.Text, "1 of 5 unread (0 addressed to you)") {
		t.Fatalf("garbage MAX must fall back to one, got:\n%s", q.Text)
	}
}
