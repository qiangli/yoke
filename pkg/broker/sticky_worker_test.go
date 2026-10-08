package broker

// Sticky bind=worker/reset=none on CLI agents: one warm CLI per binding, only
// the new turn forwarded. The broker keeps the session; cligw owns the
// process. Divergence is 409 before the worker is touched; tools that cannot
// hold a session stay 501; a full room stays 429; deleting the binding retires
// the worker.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/cligw"
)

var errTestBoom = errors.New("test boom")

type fakeStickySession struct {
	mu      sync.Mutex
	prompts []cligw.CompletionPrompt
	texts   []string
	fail    error
	closed  bool
}

func (f *fakeStickySession) Turn(_ context.Context, prompt cligw.CompletionPrompt, _ func(cligw.Event)) (cligw.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if f.fail != nil {
		return cligw.Result{Outcome: cligw.OutcomeError}, f.fail
	}
	text := "session-reply"
	if len(f.texts) > 0 {
		text = f.texts[min(len(f.prompts)-1, len(f.texts)-1)]
	}
	return cligw.Result{Text: text, Usage: cligw.Usage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4}, Outcome: cligw.OutcomeOK}, nil
}

func (f *fakeStickySession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeStickySession) turnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (c *fakeCLI) enableSticky(texts ...string) *fakeStickySession {
	sess := &fakeStickySession{texts: texts}
	c.mu.Lock()
	c.sticky = sess
	c.mu.Unlock()
	return sess
}

func (c *fakeCLI) DialSticky(_ context.Context, agent string) (stickyWorker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dials++
	if c.stickyErr != nil {
		return nil, c.stickyErr
	}
	if c.sticky == nil {
		c.sticky = &fakeStickySession{}
	}
	c.stickyAgent = agent
	return c.sticky, nil
}

func stickyTurn(t *testing.T, h *harness, key, model string, messages ...[2]string) (int, map[string]any) {
	t.Helper()
	var m []map[string]string
	for _, pair := range messages {
		m = append(m, map[string]string{"role": pair[0], "content": pair[1]})
	}
	resp, out := h.do("POST", "/sticky/"+key+"/v1/chat/completions", map[string]any{"model": model, "messages": m}, nil)
	return resp.StatusCode, out
}

func stickyContent(out map[string]any) string {
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		return ""
	}
	m, _ := choices[0].(map[string]any)["message"].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

func TestStickyWorkerTwoTurnsShareSessionAndForwardDelta(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.cli.enableSticky("reply-a", "reply-b")
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, out := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 200 || stickyContent(out) != "reply-a" {
		t.Fatalf("turn1: %d %v", status, out)
	}
	if status, out := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}, [2]string{"assistant", "reply-a"}, [2]string{"user", "B"}); status != 200 || stickyContent(out) != "reply-b" {
		t.Fatalf("turn2: %d %v", status, out)
	}
	h.cli.mu.Lock()
	dials, prompts := h.cli.dials, append([]cligw.CompletionPrompt(nil), sess.prompts...)
	h.cli.mu.Unlock()
	if dials != 1 {
		t.Fatalf("dialed %d sessions, want one warm CLI for the binding", dials)
	}
	if len(prompts) != 2 {
		t.Fatalf("turns = %d, want 2", len(prompts))
	}
	// The delta is the new turn: the echoed assistant reply plus the new user
	// message. The first user turn must not be re-sent as a user turn.
	if strings.Contains(prompts[1].Prompt, "User:\nA") || !strings.Contains(prompts[1].Prompt, "User:\nB") {
		t.Fatalf("turn2 prompt = %q (want only the new turn)", prompts[1].Prompt)
	}
}

func TestStickyWorkerDivergenceIs409BeforeTheWorker(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.cli.enableSticky()
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 200 {
		t.Fatalf("turn1: %d", status)
	}
	before := sess.turnCount()
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "DIFFERENT"}, [2]string{"assistant", "reply-a"}, [2]string{"user", "B"}); status != 409 {
		t.Fatalf("divergence: %d, want 409", status)
	}
	if got := sess.turnCount(); got != before {
		t.Fatalf("diverged request reached the worker (%d turns, want %d)", got, before)
	}
}

func TestStickyWorkerUnsupportedToolStays501(t *testing.T) {
	h := newHarness(t, nil)
	h.cli.mu.Lock()
	h.cli.stickyErr = cligw.ErrStickyUnsupported
	h.cli.mu.Unlock()
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	status, out := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"})
	if status != 501 {
		t.Fatalf("unsupported: %d %v, want 501", status, out)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "sticky") {
		t.Fatalf("501 body %s does not name the sticky reason", raw)
	}
}

func TestStickyWorkerAtCapIs429(t *testing.T) {
	h := newHarness(t, nil)
	h.cli.mu.Lock()
	h.cli.stickyErr = cligw.ErrStickyCapped
	h.cli.mu.Unlock()
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 429 {
		t.Fatalf("capped: %d, want 429", status)
	}
}

func TestStickyWorkerDeleteRetiresTheSession(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.cli.enableSticky()
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 200 {
		t.Fatalf("turn1: %d", status)
	}
	if resp, _ := h.do("DELETE", "/v1/sticky/conv", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if !closed {
		t.Fatal("binding delete left the sticky worker running")
	}
}

func TestStickyWorkerResetEachStaysOneShot(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/v1/sticky", StickySpec{Key: "each", Model: "L5", Bind: BindWorker, Reset: ResetEach}, nil)
	status, out := stickyTurn(t, h, "each", "L5", [2]string{"user", "A"})
	if status != 200 || stickyContent(out) != "cli" {
		t.Fatalf("worker/each: %d %v (want the one-shot answer)", status, out)
	}
	h.cli.mu.Lock()
	dials := h.cli.dials
	h.cli.mu.Unlock()
	if dials != 0 {
		t.Fatalf("worker/each dialed %d sticky sessions, want none", dials)
	}
}

func TestStickyWorkerFailedTurnCanBeRetried(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.cli.enableSticky("reply-a")
	sess.fail = errTestBoom
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 502 {
		t.Fatalf("failed turn: %d, want 502", status)
	}
	// The failed turn rolls its transcript back, so the retry is accepted and
	// restores the full conversation on a fresh worker instead of failing
	// with an empty delta or silently resetting it.
	h.cli.mu.Lock()
	h.cli.sticky = &fakeStickySession{texts: []string{"reply-a"}}
	h.cli.mu.Unlock()
	status, out := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"})
	if status != 200 || stickyContent(out) != "reply-a" {
		t.Fatalf("retry: %d %v", status, out)
	}
	h.cli.mu.Lock()
	next, dials := h.cli.sticky, h.cli.dials
	h.cli.mu.Unlock()
	if dials != 2 {
		t.Fatalf("retry dialed %d sessions, want a fresh worker", dials)
	}
	next.mu.Lock()
	restored := len(next.prompts) == 1 && strings.Contains(next.prompts[0].Prompt, "A")
	next.mu.Unlock()
	if !restored {
		t.Fatal("retry did not restore the conversation on the fresh worker")
	}
}
