package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCoachEscalatesAfterGenericSteers(t *testing.T) {
	// P2b: after EscalateAfter (2) generic steers, the reflex hands off to the
	// agent-coach on the next trip, with the coachee + recent context.
	fs := &fakeSteerer{}
	c := NewLineCoach(DefaultCoachPolicy(), fs) // EscalateAfter 2, MaxSteers 3
	got := make(chan EscalationRequest, 1)
	c.SetEscalation(context.Background(), "codex-gpt-5.5", func(ctx context.Context, req EscalationRequest) (string, string, bool) {
		got <- req
		return "the two tests are contradictory; report that instead of retrying", "claude-fable5", true
	})
	acts := []string{"run_tests on ./x", "read foo.go again", "still failing here", "let me try once more"}
	for i := 0; i < 240; i++ {
		_, _ = c.Write([]byte(acts[i%len(acts)] + "\n"))
	}
	select {
	case req := <-got:
		if req.Coachee != "codex-gpt-5.5" {
			t.Errorf("coachee = %q, want codex-gpt-5.5", req.Coachee)
		}
		if req.Trip != 3 {
			t.Errorf("escalation trip = %d, want 3 (after 2 generic steers)", req.Trip)
		}
		if len(req.Recent) == 0 {
			t.Errorf("escalation must carry the recent output for context")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent-coach was never escalated to after the generic steers")
	}
	// the content-full steer should reach the agent (async).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !fs.saidContains("contradictory") {
		time.Sleep(20 * time.Millisecond)
	}
	if !fs.saidContains("contradictory") {
		t.Errorf("escalated steer not injected into the agent")
	}
}

func TestPickCoachAgentIsAboveAndNotSelf(t *testing.T) {
	// Light live-catalog check: a coach is >= target band and never the coachee.
	cat := newCatalog()
	b := coacheeBand(cat, "ycode-glm-5.2")
	if b == 0 {
		t.Skip("catalog did not resolve a band in this env")
	}
	coach := pickCoachAgent(cat, b+1, "ycode-glm-5.2")
	if coach == "ycode-glm-5.2" {
		t.Errorf("a coach must never be the coachee itself")
	}
	if coach != "" {
		if cb := coacheeBand(cat, coach); cb < b+1 {
			t.Errorf("coach %q band %d is below target %d", coach, cb, b+1)
		}
	}
}

func TestNoteCoachAgenticEmitsStructuredAdvice(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "1")
	c := newCoach(DefaultCoachPolicy())
	c.total = 40
	c.steers = append(c.steers, SteerRecord{Reason: "churn"})
	var buf bytes.Buffer
	NoteCoach(c, &buf)
	if !strings.Contains(buf.String(), "bashy-advice-v1") || !strings.Contains(buf.String(), "agent-loop") {
		t.Fatalf("agentic NoteCoach must emit a bashy-advice-v1 agent-loop line, got: %q", buf.String())
	}
}

func TestNoteCoachHumanEmitsProse(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	c := newCoach(DefaultCoachPolicy())
	c.total = 40
	c.steers = append(c.steers, SteerRecord{Reason: "churn"})
	var buf bytes.Buffer
	NoteCoach(c, &buf)
	if !strings.Contains(buf.String(), "[coach]") || strings.Contains(buf.String(), "bashy-advice-v1") {
		t.Fatalf("non-agentic NoteCoach must emit a prose line, got: %q", buf.String())
	}
}

// toolCall builds a tool.call event with the given name+input.
func toolCall(name, input string) Event {
	data, _ := json.Marshal(map[string]any{"name": name, "input": json.RawMessage(input)})
	return Event{Type: EventToolCall, Data: data}
}

// feed runs a sequence of calls through decide and returns how many steers fired.
func feed(c *Coach, calls []Event) int {
	n := 0
	for _, ev := range calls {
		if c.decide(ev) != nil {
			n++
		}
	}
	return n
}

func TestCoachTripsOnRepeatedIdenticalCall(t *testing.T) {
	// The glm/kimi-k3 failure: the SAME call, over and over. distinct stays 1.
	pol := DefaultCoachPolicy() // RepeatThreshold 3, MinCalls 3
	c := newCoach(pol)
	same := toolCall("go_test", `{"pkg":"./..."}`)
	steers := feed(c, []Event{same, same, same, same, same})
	if steers != 1 {
		t.Fatalf("want exactly 1 steer (fires at the 3rd identical call, then cooldown holds it), got %d", steers)
	}
	rep := c.Report()
	if rep.Steers[0].Reason != "repeat" {
		t.Errorf("reason = %q, want repeat", rep.Steers[0].Reason)
	}
	if rep.Distinct != 1 || rep.Total != 5 {
		t.Errorf("total/distinct = %d/%d, want 5/1", rep.Total, rep.Distinct)
	}
}

func TestCoachDoesNotTripOnHealthyDistinctWork(t *testing.T) {
	// Every call different: real progress. A coach must never touch this.
	pol := DefaultCoachPolicy()
	c := newCoach(pol)
	var calls []Event
	for i := 0; i < 12; i++ {
		calls = append(calls, toolCall("read_file", `{"path":"f`+string(rune('a'+i))+`.go"}`))
	}
	if n := feed(c, calls); n != 0 {
		t.Fatalf("healthy distinct work must never trip the coach, got %d steers", n)
	}
}

func TestCoachRespectsMaxSteersAndCooldown(t *testing.T) {
	// A run that keeps looping should still be steered only up to MaxSteers, and
	// only after Cooldown distinct calls between interventions.
	pol := DefaultCoachPolicy()
	pol.MaxSteers = 2
	pol.Cooldown = 2
	c := newCoach(pol)
	// First loop on call A -> 1 steer. Then 2 new distinct calls (satisfies
	// cooldown), then loop on B -> 2nd steer. Then loop on C -> capped, no 3rd.
	a := toolCall("t", `{"k":"a"}`)
	b := toolCall("t", `{"k":"b"}`)
	c1 := toolCall("t", `{"k":"c1"}`)
	c2 := toolCall("t", `{"k":"c2"}`)
	d := toolCall("t", `{"k":"d"}`)
	seq := []Event{a, a, a /*steer1*/, c1, c2 /*cooldown met*/, b, b, b /*steer2*/, d, d, d /*capped*/}
	if n := feed(c, seq); n != 2 {
		t.Fatalf("want 2 steers (MaxSteers cap), got %d", n)
	}
}

// feedLines runs raw terminal lines through the pty detector, returns steer count.
func feedLines(c *Coach, lines []string) int {
	n := 0
	for _, ln := range lines {
		if c.feedPty(ln) != nil {
			n++
		}
	}
	return n
}

func TestPtyNormalizeScrubsVolatile(t *testing.T) {
	// A spinner's changing timer/counter must collapse to ONE key, or it reads as
	// new content every frame and masks the loop.
	a := normalizeLine("\x1b[32m▸ Thought for 5s, 386 tokens\x1b[0m")
	b := normalizeLine("▸ Thought for 12s, 1904 tokens")
	if a != b {
		t.Errorf("volatile tokens not scrubbed to a common key:\n  %q\n  %q", a, b)
	}
}

func TestPtyIgnoresRedrawsAndHealthyWork(t *testing.T) {
	c := newCoach(DefaultCoachPolicy())
	var lines []string
	// In-place redraws of the SAME line (a spinner) — consecutive, must collapse.
	for i := 0; i < 30; i++ {
		lines = append(lines, "Working on the task, please wait")
	}
	// Then genuinely distinct progress — every line new.
	for i := 0; i < 60; i++ {
		lines = append(lines, "editing distinct source file number "+string(rune('a'+i%26))+"xyz")
	}
	if n := feedLines(c, lines); n != 0 {
		t.Fatalf("redraws + healthy distinct work must not trip pty coach, got %d steers", n)
	}
}

func TestPtyDetectsChurnLoop(t *testing.T) {
	c := newCoach(DefaultCoachPolicy()) // window 40, novelty floor 0.35
	var lines []string
	// A churn loop: the agent cycles through the SAME 4 actions over and over,
	// interleaved (not consecutive, so dedup won't hide it). 4 distinct / 40 window
	// = novelty 0.10, well below 0.35.
	acts := []string{
		"run_tests on package ./foobar",
		"read_file internal/median/median.go",
		"the tests are still failing here",
		"let me check the implementation again",
	}
	for i := 0; i < 60; i++ {
		lines = append(lines, acts[i%len(acts)])
	}
	if n := feedLines(c, lines); n < 1 {
		t.Fatalf("a churn loop (4 distinct actions cycling) must trip the pty coach, got %d", n)
	}
	if c.Report().Steers[0].Reason != "churn" {
		t.Errorf("reason = %q, want churn", c.Report().Steers[0].Reason)
	}
}

func TestPtyReportHasCumulativeDistinct(t *testing.T) {
	// Regression: the report must show pty-mode distinct (cumulative lines), not
	// the event map — which read 0 and looked like the coach saw nothing.
	c := newCoach(DefaultCoachPolicy())
	for i := 0; i < 20; i++ {
		c.feedPty("distinct progress line number " + string(rune('a'+i)) + "abcdef")
	}
	rep := c.Report()
	if rep.Total != 20 || rep.Distinct != 20 {
		t.Fatalf("pty report total/distinct = %d/%d, want 20/20", rep.Total, rep.Distinct)
	}
}

// fakeSteerer records what a coach did, so the weave-style path can be tested
// without a live agent or a control socket. Thread-safe: the escalation path Says
// from a goroutine.
type fakeSteerer struct {
	mu         sync.Mutex
	interrupts int
	says       []string
}

func (f *fakeSteerer) Interrupt() error { f.mu.Lock(); f.interrupts++; f.mu.Unlock(); return nil }
func (f *fakeSteerer) Say(text string) error {
	f.mu.Lock()
	f.says = append(f.says, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeSteerer) saidCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.says) }
func (f *fakeSteerer) saidContains(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.says {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestLineCoachWriteSteersOnChurn(t *testing.T) {
	// This is exactly the weave reflex path: a coach fed a run's output via Write,
	// steering through a Steerer (not a Session). A churning stream must produce
	// an ESC + a Say.
	fs := &fakeSteerer{}
	c := NewLineCoach(DefaultCoachPolicy(), fs)
	acts := []string{
		"run_tests on package ./svc\n",
		"read_file internal/svc/svc.go\n",
		"the tests are still failing here\n",
		"let me check the implementation again\n",
	}
	for i := 0; i < 60; i++ {
		if _, err := c.Write([]byte(acts[i%len(acts)])); err != nil {
			t.Fatal(err)
		}
	}
	if len(fs.says) < 1 {
		t.Fatalf("weave reflex path must steer on a churn stream, got %d says", len(fs.says))
	}
	if fs.interrupts < 1 {
		t.Errorf("ESC (Interrupt) should precede the Say, got %d interrupts", fs.interrupts)
	}
}

func TestLineCoachWriteQuietOnHealthyStream(t *testing.T) {
	fs := &fakeSteerer{}
	c := NewLineCoach(DefaultCoachPolicy(), fs)
	for i := 0; i < 60; i++ {
		line := "editing a distinct file path number " + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "z\n"
		_, _ = c.Write([]byte(line))
	}
	if len(fs.says) != 0 {
		t.Fatalf("healthy distinct stream must not steer, got %d says", len(fs.says))
	}
}

func TestPtyErrorRateCatchesFailingThrash(t *testing.T) {
	// Distinct steps (no churn), no repetition, but EVERY step fails. Only the
	// error-rate signal can catch this — the gap repetition misses.
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget, pol.SoftStepBudget, pol.SoftTimeBudget = 0, 0, 0 // isolate error-rate
	c := NewLineCoach(pol, &fakeSteerer{})
	for i := 0; i < 40; i++ {
		_, _ = c.Write([]byte("build failed: cannot compile module_" +
			string(rune('a'+i%26)) + string(rune('a'+i/26)) + "\n"))
	}
	steers := c.Report().Steers
	if len(steers) == 0 || steers[0].Reason != "error-rate" {
		t.Fatalf("failing-but-distinct thrash must trip error-rate, got %v", steers)
	}
}

func TestPtyBudgetCatchesBruteForce(t *testing.T) {
	// The junior brute-forcer: distinct steps, NO errors, no repetition, no
	// progress. Churn and error-rate stay silent; only the budget catches it.
	pol := DefaultCoachPolicy()
	pol.SoftStepBudget = 30
	pol.SoftTokenBudget, pol.SoftTimeBudget, pol.ErrorRateThreshold = 0, 0, 0 // isolate step budget
	c := NewLineCoach(pol, &fakeSteerer{})
	for i := 0; i < 35; i++ {
		_, _ = c.Write([]byte("trying a distinct combination approach variant " +
			string(rune('a'+i%26)) + string(rune('a'+i/26)) + "xy\n"))
	}
	steers := c.Report().Steers
	if len(steers) == 0 || steers[0].Reason != "budget-steps" {
		t.Fatalf("brute-force (distinct, error-free) must trip the step budget, got %v", steers)
	}
}

func TestPtyBudgetCostTripsOnTokenBurn(t *testing.T) {
	// Cost-first budget: a run generating a lot of output trips budget-cost even
	// with distinct, error-free lines — the spend/quota signal.
	pol := DefaultCoachPolicy()
	pol.SoftTokenBudget = 100 // ~100 output tokens ≈ 400 bytes
	pol.SoftStepBudget, pol.SoftTimeBudget, pol.ErrorRateThreshold = 0, 0, 0
	c := NewLineCoach(pol, &fakeSteerer{})
	for i := 0; i < 25; i++ {
		_, _ = c.Write([]byte("distinct progress output line making steady headway variant " +
			string(rune('a'+i)) + "\n"))
	}
	steers := c.Report().Steers
	if len(steers) == 0 || steers[0].Reason != "budget-cost" {
		t.Fatalf("token/quota burn must trip budget-cost, got %v", steers)
	}
}

func TestCoachTripsOnRatioAcrossFewCalls(t *testing.T) {
	// A loop spread across two calls: never 3 of ONE, but ratio climbs.
	pol := CoachPolicy{RepeatThreshold: 99, RatioThreshold: 3.0, MinCalls: 3, MaxSteers: 3, Cooldown: 1}
	c := newCoach(pol)
	a := toolCall("t", `{"k":"a"}`)
	b := toolCall("t", `{"k":"b"}`)
	// a,b,a,b,a,b -> at the 6th call total=6 distinct=2 ratio=3.0 -> trip
	if n := feed(c, []Event{a, b, a, b, a, b}); n < 1 {
		t.Fatalf("ratio loop should trip at least once, got %d", n)
	}
	if c.Report().Steers[0].Reason != "ratio" {
		t.Errorf("reason = %q, want ratio", c.Report().Steers[0].Reason)
	}
}
