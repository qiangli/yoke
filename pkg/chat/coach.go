package chat

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/telemetry"
)

// LIVE AGENT COACHING — P0, the LLM-free auto-coach.
//
// A coach watches a running session's tool.call stream and, when the agent
// starts LOOPING — re-issuing calls without making new distinct progress —
// intervenes to break it out and tell it to deliver. It is the runtime twin of
// the space-time advisor: the advisor explains a FAILED command after the fact;
// the coach steers a doomed loop WHILE it is running.
//
// It is a REPORT CHANNEL, NEVER AN AUTHOR. A coach can press ESC and say a
// sentence; it cannot write a file or merge. That boundary is the whole reason
// it is safe to point one agent at another's live session.
//
// Why ESC and not just a sentence: every agent TUI in this fleet queues a Say
// and reads it only between turns. An agent stuck in a tool loop never reaches
// the between-turns moment, so the sentence sits unread forever. Escape is the
// only thing that reaches it there (foreman proved this live: an agy conductor
// made 224 tool calls, 22 distinct, and only ESC could stop it). So the coach
// interrupts first, THEN speaks.
//
// The signal is deliberately CHEAP and LLM-free: distinct=1 with a climbing
// count is exactly the glm-5.2 / kimi-k3 non-convergence failure the fleet keeps
// measuring. No model call is needed to see it, and none should be spent.

// CoachPolicy configures the auto-coach.
type CoachPolicy struct {
	// RepeatThreshold trips when ONE (tool,input) has been issued this many
	// times. The cheapest non-convergence signal there is.
	RepeatThreshold int
	// RatioThreshold trips when total/distinct reaches this (once MinCalls is
	// met). Catches loops that spread across a handful of calls.
	RatioThreshold float64
	// MinCalls suppresses any trip before this many total calls — do not coach a
	// run that has barely started.
	MinCalls int
	// Cooldown suppresses a re-steer until this many NEW distinct calls have
	// happened since the last one. Sparse by construction: an over-eager coach
	// collapses the worker's own reasoning.
	Cooldown int
	// MaxSteers is a hard cap on interventions per session.
	MaxSteers int
	// EscalateAfter is how many GENERIC reflex steers to try before escalating to
	// an agent-coach (P2b). 0 = never escalate (reflex only). The escalation is
	// one-shot and needs both an EscalateFunc and a live steerer. It must be <
	// MaxSteers, so the escalation trip is not swallowed by the cap.
	EscalateAfter int
	// Steer is the line injected after the interrupt.
	Steer string
	// Interrupt sends ESC before the Steer. On by default; the only reason to
	// disable it is a probe of whether a plain Say lands (it does not, mid-loop).
	Interrupt bool
	// LogPath, if set, receives one JSON line per steer — the (state -> steer)
	// record that seeds the training loop (P3).
	LogPath string

	// --- pty-scrape mode (event-less tools: agy, and any third-party CLI) ---
	// When the tool has no event channel, the coach cannot see tool.call as data.
	// It falls back to a GENERIC signal over the terminal output: a loop is
	// "output flowing but distinct content not growing" — the pty analog of the
	// repeat ratio, needing no per-TUI syntax. These tune that detector.
	//
	// PtyWindow is the sliding window of recent significant output lines.
	PtyWindow int
	// PtyNoveltyFloor trips when distinct/window (the novelty ratio) falls to or
	// below this — i.e. the last PtyWindow lines are mostly repeats of each other.
	// Healthy work runs ~0.7–1.0; a churning loop collapses toward 0.
	PtyNoveltyFloor float64

	// --- error-rate signal: thrash where each step DIFFERS but all FAIL ---
	// This catches the loop repetition misses (varying edits, every test red).
	// ErrorRateThreshold trips when the recent error fraction reaches it (0 = off).
	ErrorRateThreshold float64
	// ErrorWindow is how many recent significant lines the error fraction is over.
	ErrorWindow int

	// --- budget-box: "you are burning budget without converging" ---
	// This catches the junior that tries combinations forever: distinct steps, no
	// errors, no progress — nothing else fires. It RE-FIRES at each budget
	// increment (not once), so a persistent grinder accumulates trips toward the
	// agent-coach, who can name the structural problem. All three budgets nudge; a
	// nudge to "converge or report" is benign even on a legitimately long task.
	//
	// Cost is the economical signal and it is measurable mid-stream, so it comes
	// FIRST: SoftTokenBudget is estimated OUTPUT tokens (~bytes/4) per increment —
	// a proxy for spend that the coach can see without an API usage feed (0 = off).
	// A precise cost signal (real input+output usage from the gateway) is a later
	// refinement fed in from the API layer.
	SoftTokenBudget int
	// SoftTimeBudget re-fires at each elapsed-wall-clock increment (0 = off). Wire
	// it to a fraction of the run's watchdog so the coach nudges before the kill.
	SoftTimeBudget time.Duration
	// SoftStepBudget re-fires at each N significant output lines (0 = off) — a
	// deterministic, env-free backstop for runaway output.
	SoftStepBudget int
}

// DefaultCoachPolicy is the P0 "you have the answer, stop" coach.
func DefaultCoachPolicy() CoachPolicy {
	return CoachPolicy{
		RepeatThreshold: 3,
		RatioThreshold:  3.0,
		MinCalls:        3,
		Cooldown:        2,
		MaxSteers:       3,
		EscalateAfter:   2, // 2 generic steers, then escalate to an agent-coach
		Interrupt:       true,
		Steer:           "You appear to be repeating work you have already completed. If you already have the answer, STOP investigating and deliver your final result now.",
		PtyWindow:       40,
		PtyNoveltyFloor: 0.35,

		ErrorRateThreshold: 0.6,
		ErrorWindow:        20,
		SoftTokenBudget:    50000, // ~50k output tokens per nudge — spend/quota proxy
		SoftTimeBudget:     0,     // wired to a fraction of the run's watchdog by the caller
		SoftStepBudget:     400,   // backstop for runaway output
	}
}

// Steer messages, per signal. churn/repeat use the policy's Steer.
const (
	errorRateSteer = "Most of your recent actions are FAILING. The current approach is not working — stop, reconsider whether the goal is achievable as stated, and either try a fundamentally different approach or report the blocker instead of retrying."
	budgetSteer    = "You have burned a large part of your budget (tokens/quota/time) without converging. If you are exploring options without a plan, STOP and reason about the structure of the problem; then deliver your best result or report clearly what is blocking you."
)

// SteerRecord is one intervention: the signal that triggered it and what was said.
type SteerRecord struct {
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`  // "repeat" | "ratio"
	Trigger  string    `json:"trigger"` // the looping call: tool|inputhash
	Count    int       `json:"count"`   // times that call had been issued
	Total    int       `json:"total"`
	Distinct int       `json:"distinct"`
	Repeat   float64   `json:"repeat"`
	Steer    string    `json:"steer"`
	Agent    string    `json:"agent"`
	Coach    string    `json:"coach,omitempty"` // the agent-coach that produced an escalated steer (P2b)
}

// CoachReport summarizes a session after it ends.
type CoachReport struct {
	Total    int           `json:"total_calls"`
	Distinct int           `json:"distinct_calls"`
	Repeat   float64       `json:"repeat_ratio"`
	Steers   []SteerRecord `json:"steers"`
}

// Steerer is the minimal control surface a coach needs to intervene: break the
// current turn (ESC) and inject a line. *Session implements it; so does any run
// with an agentpty control socket (weave), via NewCtlSteerer.
type Steerer interface {
	Interrupt() error
	Say(text string) error
}

var _ Steerer = (*Session)(nil)

// ctlSteerer steers a run straight through its agentpty control socket — the
// weave path, where there is no chat.Session, only a ctlSock.
type ctlSteerer struct{ ctlSock string }

// NewCtlSteerer builds a Steerer over an agentpty control socket. A "" socket
// makes every intervention a no-op (detect-and-log without steering).
func NewCtlSteerer(ctlSock string) Steerer { return ctlSteerer{ctlSock: ctlSock} }

func (s ctlSteerer) Interrupt() error {
	if s.ctlSock == "" {
		return nil
	}
	return agentpty.SendFrame(s.ctlSock, agentpty.VerbatimFrame([]byte{0x1b}))
}

func (s ctlSteerer) Say(text string) error {
	if s.ctlSock == "" {
		return nil
	}
	return agentpty.BrokerSay(s.ctlSock, text)
}

// Coach is a live, LLM-free watcher over an agent run's tool.call stream (event
// mode) or terminal output (pty mode).
type Coach struct {
	sess  *Session
	steer Steerer
	agent string
	pol   CoachPolicy
	mode  string // "events" or "pty"

	mu             sync.Mutex
	counts         map[string]int // (tool|inputhash) -> times seen
	total          int
	priorCounts    map[string]struct{} // calls/lines present before an attached watch began
	priorTotal     int
	distinctAtLast int // distinct count when we last steered
	steers         []SteerRecord
	done           chan struct{}

	// pty-scrape mode state
	ptyOffset  int                 // byte cursor into Session.Output()
	ptyPartial string              // trailing incomplete line carried between polls
	ptyLast    string              // last KEPT normalized line (consecutive-dedup)
	window     []string            // sliding window of significant normalized lines
	winCount   map[string]int      // multiset count for the window
	ptySeen    map[string]struct{} // CUMULATIVE distinct lines, for the report
	recent     []string            // rolling recent significant lines, for escalation context

	// signal-panel state
	errWin     []bool    // recent significant lines: was each an error line?
	errCount   int       // errors currently in errWin
	outBytes   int       // cumulative output bytes, for the token/spend estimate
	started    time.Time // first-line time, for the wall-clock budget
	tokenMarks int       // token-budget increments already fired
	timeMarks  int       // time-budget increments already fired
	stepMarks  int       // step-budget increments already fired

	// P2b escalation
	escalate  EscalateFunc    // the agent-coach; nil = reflex only
	escCtx    context.Context // context for the (slow) agent-coach invoke
	coachee   string          // the looping agent's binding, for band lookup
	escalated bool            // one-shot: escalate at most once

	// unresolvedRaised latches the supervisor alert to ONE per session — see
	// noteUnresolved. A notification wants the transition, not every occurrence.
	unresolvedRaised bool
}

// SetEscalation arms the band-graduated agent coach (P2b): after EscalateAfter
// generic steers fail to break the loop, the reflex invokes esc — an agent one
// band above coachee — for a content-full steer. coachee is the looping agent's
// binding (for the band lookup); ctx bounds the agent-coach invoke. A nil esc or
// an empty coachee leaves the coach reflex-only.
func (c *Coach) SetEscalation(ctx context.Context, coachee string, esc EscalateFunc) {
	c.mu.Lock()
	c.escCtx = ctx
	c.coachee = coachee
	c.escalate = esc
	c.mu.Unlock()
}

// newCoach builds a coach with no session attached — the form the signal test
// drives directly, feeding it events and asserting the trip decision without
// any live agent or socket IO.
func newCoach(pol CoachPolicy) *Coach {
	return &Coach{pol: pol, mode: "events", counts: map[string]int{}, winCount: map[string]int{}, ptySeen: map[string]struct{}{}, done: make(chan struct{})}
}

// NewLineCoach builds a coach fed one line at a time (its Write method) and
// steering through the given Steerer — the form weave uses: it tees a run's
// decoded output into the coach and lets the pty-novelty detector run over it,
// with no chat.Session involved. Always pty-mode.
func NewLineCoach(pol CoachPolicy, steer Steerer) *Coach {
	c := newCoach(pol)
	c.steer = steer
	c.mode = "pty"
	return c
}

// Write feeds streamed output to the pty detector, line by line. It is an
// io.Writer so a caller can `io.MultiWriter(log, coach)` a run's output into it.
// Safe to tee from more than one stream (e.g. a pipe run's stdout AND stderr):
// the partial-line buffer is guarded, and feedPty locks the shared counters.
func (c *Coach) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.outBytes += len(p) // ALL output bytes, for the token/spend estimate
	c.ptyPartial += string(p)
	idx := strings.LastIndexByte(c.ptyPartial, '\n')
	if idx < 0 {
		c.mu.Unlock()
		return len(p), nil // no complete line yet
	}
	complete := c.ptyPartial[:idx]
	c.ptyPartial = c.ptyPartial[idx+1:]
	c.mu.Unlock()
	for _, ln := range strings.Split(complete, "\n") {
		if rec := c.feedPty(ln); rec != nil {
			c.intervene(rec)
		}
	}
	return len(p), nil
}

// ReflexEnabled is the default-on gate for the auto-attached reflex coach in the
// delegation verbs (weave/invoke/delegate). On unless BASHY_NO_COACH is truthy —
// loop protection is a property of delegation, not a verb to remember.
func ReflexEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_NO_COACH"))) {
	case "1", "true", "yes", "on":
		return false
	}
	return true
}

// StartCoach attaches a coach to a running session and begins watching. It
// returns immediately; the coach runs until the context ends. Call Wait to
// block for the watcher to drain after cancelling.
func (s *Session) StartCoach(ctx context.Context, pol CoachPolicy) *Coach {
	c := newCoach(pol)
	c.sess = s
	c.steer = s
	c.agent = s.Agent
	go c.watch(ctx)
	return c
}

func (c *Coach) watch(ctx context.Context) {
	defer close(c.done)
	if c.sess == nil {
		return
	}
	if c.sess.EventsPath() != "" {
		c.mu.Lock()
		c.mode = "events"
		c.mu.Unlock()
		c.watchEvents(ctx) // precise: structured tool.call stream (ycode)
		return
	}
	c.mu.Lock()
	c.mode = "pty"
	c.mu.Unlock()
	c.watchPty(ctx) // generic fallback: loop-from-terminal-output (agy & any pty CLI)
}

// PtyMode reports whether this coach is watching the terminal output (no event
// channel) rather than the structured tool.call stream.
func (c *Coach) PtyMode() bool { return c.sess != nil && c.sess.EventsPath() == "" }

// watchEvents follows the tool's NDJSON event file — the precise path.
func (c *Coach) watchEvents(ctx context.Context) {
	// An INDEPENDENT tail: its own offset, so it never races the session's own
	// eventTail (which WaitIdle drains). Two readers of one append-only file.
	tail := &eventTail{path: c.sess.EventsPath()}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if evs, err := tail.drain(); err == nil {
			for _, ev := range evs {
				if ev.Type == EventToolCall {
					c.onToolCall(ev)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// watchPty is the GENERIC, event-less signal. It polls the accumulating terminal
// scrape, normalizes each new line, and feeds the significant ones to a novelty
// detector. No per-TUI syntax: a loop is "output flowing, distinct content not
// growing", which every agent CLI exhibits when it churns.
func (c *Coach) watchPty(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		full := c.sess.Output()
		if len(full) > c.ptyOffset {
			chunk := c.ptyPartial + full[c.ptyOffset:]
			c.ptyOffset = len(full)
			lines := strings.Split(chunk, "\n")
			c.ptyPartial = lines[len(lines)-1] // last segment is still being written
			for _, ln := range lines[:len(lines)-1] {
				if rec := c.feedPty(ln); rec != nil {
					c.intervene(rec)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// feedPty runs one raw terminal line through the SIGNAL PANEL and returns a
// SteerRecord when any signal trips (else nil). The panel: churn (repetition),
// error-rate (varying-but-failing), and the budget (cost/quota/time/steps —
// "burning budget without converging", the catch-all for a brute-forcer that
// makes distinct, error-free, progress-free moves). All share the trip machinery.
func (c *Coach) feedPty(raw string) *SteerRecord {
	norm := normalizeLine(raw)
	if !significant(norm) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started.IsZero() {
		c.started = time.Now()
	}
	if norm == c.ptyLast {
		return nil // an in-place redraw of the same line — not progress, not a loop
	}
	c.ptyLast = norm

	c.ptySeen[norm] = struct{}{} // cumulative, for the report
	c.recent = append(c.recent, norm)
	if len(c.recent) > 30 {
		c.recent = c.recent[len(c.recent)-30:]
	}
	c.window = append(c.window, norm)
	c.winCount[norm]++
	c.total++
	c.pushErr(isErrorLine(norm))
	w := c.pol.PtyWindow
	if w <= 0 {
		w = 40
	}
	if len(c.window) > w {
		old := c.window[0]
		c.window = c.window[1:]
		if c.winCount[old]--; c.winCount[old] <= 0 {
			delete(c.winCount, old)
		}
	}
	if len(c.steers) >= c.pol.MaxSteers {
		return nil
	}
	distinct := len(c.winCount)

	// signal 1 — CHURN: the window has collapsed into repetition.
	if len(c.window) >= w {
		floor := c.pol.PtyNoveltyFloor
		if floor <= 0 {
			floor = 0.35
		}
		if float64(distinct)/float64(len(c.window)) <= floor {
			return c.tripPty("churn", mostRepeated(c.winCount), c.pol.Steer, distinct, int64(len(c.window)), int64(distinct))
		}
	}
	// signal 2 — ERROR-RATE: distinct steps, but most are failing.
	if c.pol.ErrorRateThreshold > 0 && len(c.errWin) >= c.errWindow() &&
		float64(c.errCount)/float64(len(c.errWin)) >= c.pol.ErrorRateThreshold {
		return c.tripPty("error-rate", "errors", errorRateSteer, distinct, int64(len(c.errWin)), int64(c.errCount))
	}
	// signal 3 — BUDGET: burning cost/quota/time/steps without converging. Cost
	// first (mid-stream, and what an api-key OR subscription-quota run actually
	// spends); re-fires at each increment so a grinder escalates.
	if b := c.pol.SoftTokenBudget; b > 0 && c.outBytes/4 >= (c.tokenMarks+1)*b {
		c.tokenMarks++
		return c.tripPty("budget-cost", "output-tokens", budgetSteer, distinct, int64(b), int64(c.outBytes/4))
	}
	if b := c.pol.SoftTimeBudget; b > 0 && !c.started.IsZero() &&
		time.Since(c.started) >= time.Duration(c.timeMarks+1)*b {
		c.timeMarks++
		return c.tripPty("budget-time", "elapsed", budgetSteer, distinct, int64(b.Seconds()), int64(time.Since(c.started).Seconds()))
	}
	if b := c.pol.SoftStepBudget; b > 0 && c.total >= (c.stepMarks+1)*b {
		c.stepMarks++
		return c.tripPty("budget-steps", "steps", budgetSteer, distinct, int64(b), int64(c.total))
	}
	return nil
}

// tripPty builds a steer record, records the trip as an OTel BoundHit (a limit
// that BOUND — the coach's whole reason to exist is that a loop/budget limit
// bound silently), and resets the per-window signal state (churn + error) so each
// must rebuild before firing again. Budget marks persist (they count total burn),
// so a budget signal re-fires on schedule. Caller holds c.mu.
func (c *Coach) tripPty(reason, trigger, steerMsg string, distinct int, limit, actual int64) *SteerRecord {
	rec := SteerRecord{
		At: time.Now().UTC(), Reason: reason, Trigger: trigger, Count: c.errCount,
		Total: c.total, Distinct: distinct, Repeat: ratioOf(len(c.window), distinct),
		Steer: steerMsg, Agent: c.agent,
	}
	c.steers = append(c.steers, rec)
	telemetry.BoundHit(c.telCtx(), "coach."+reason, limit, actual, c.coachee)
	// Top of the ladder: when the reflex + agent-coach are exhausted and the loop
	// PERSISTS, raise a distinct SUPERVISOR ALERT — a bound the conductor/steward/
	// foreman must see and act on (kill, reassign, re-scope). The coach reports; it
	// does not author the action (report/author split).
	c.noteUnresolved()
	c.window = c.window[:0]
	c.winCount = map[string]int{}
	c.ptyLast = ""
	c.errWin = c.errWin[:0]
	c.errCount = 0
	return &rec
}

// noteUnresolved raises the supervisor alert: the intervention ladder is
// exhausted and the loop PERSISTS.
//
// Shared by both detection modes, and it did not used to be. The alert lived
// inline in tripPty, the pty-scrape path — so an events-mode agent (a first-party
// harness reporting tool.call as data) could loop past MaxSteers and raise
// NOTHING, no telemetry and no notification, even though Unresolved() reported
// true for it. The condition is mode-independent, so the alert has to be too.
//
// It emits on two channels, deliberately, because they answer different
// questions. OTel RECORDS WHAT HAPPENED for a human reading a dashboard later —
// but by the time anyone reads a dashboard, the looping agent has burned the
// budget the alert was warning about. The bus TELLS AN INTERESTED AGENT TO ACT:
// the conductor, steward or foreman who can kill, reassign or re-scope this run.
// Observability and coordination fan out from the same trip.
//
// The bus notification requests the INTERRUPT tier, because a loop that both the
// reflex and the agent-coach failed to resolve is a direction-changer for whoever
// is supervising — the definition of what may push rather than queue. It stays a
// request: a subscriber names whose interrupts it honours, and one that has not
// named the coach receives this queued instead. Reporting an urgency is not
// authoring an action, so the report/author split the ladder already observes
// holds here too.
//
// Latched to fire ONCE per session. Both callers happen to be guarded — each trip
// path stops at MaxSteers — but the latch makes the once-ness a property of the
// alert itself rather than of whatever guard happens to sit above it. A
// notification wants the TRANSITION; a metric would want every occurrence, and if
// that guard ever moves, an unlatched alert would spray the bus for as long as the
// agent kept looping: the pollution failure mode, produced by the very signal
// meant to rescue it.
//
// Best-effort on the bus: a store that cannot be written must never break
// coaching. Caller holds c.mu.
func (c *Coach) noteUnresolved() {
	if c.unresolvedRaised || c.pol.MaxSteers <= 0 || len(c.steers) < c.pol.MaxSteers {
		return
	}
	c.unresolvedRaised = true

	alert := c.coachee + ": persistent loop; reflex+agent-coach did not resolve — supervisor attention needed"
	telemetry.BoundHit(c.telCtx(), "coach.unresolved", int64(c.pol.MaxSteers), int64(len(c.steers)), alert)
	_ = bus.Publish(bus.Notification{
		Principal: "coach",
		Topic:     "fleet.unresolved",
		Body:      alert,
		Priority:  bus.DeliveryInterrupt,
	})
}

// telCtx is the context the coach emits telemetry through — the run's context
// when escalation armed it, else Background (BoundHit/Provenance record on a
// standalone span either way; they never silently drop).
func (c *Coach) telCtx() context.Context {
	if c.escCtx != nil {
		return c.escCtx
	}
	return context.Background()
}

func (c *Coach) errWindow() int {
	if c.pol.ErrorWindow > 0 {
		return c.pol.ErrorWindow
	}
	return 20
}

// pushErr slides one line into the error window. Caller holds c.mu.
func (c *Coach) pushErr(isErr bool) {
	c.errWin = append(c.errWin, isErr)
	if isErr {
		c.errCount++
	}
	if len(c.errWin) > c.errWindow() {
		if c.errWin[0] {
			c.errCount--
		}
		c.errWin = c.errWin[1:]
	}
}

func mostRepeated(counts map[string]int) string {
	trigger, hi := "", 0
	for k, n := range counts {
		if n > hi {
			trigger, hi = k, n
		}
	}
	return trigger
}

// errMarkers matches a line that reports a failure — the signal that an agent's
// distinct-but-fruitless steps are actually erroring, not progressing.
var errMarkers = regexp.MustCompile(`(?i)\b(error|errors|fail|failed|failing|failure|panic|traceback|exception|fatal|refused|denied|timed out|timeout)\b`)

func isErrorLine(norm string) bool { return errMarkers.MatchString(norm) }

// toolCallData is the payload we key on: same name AND same input is the same
// call. Same tool with different args is progress, not a loop.
type toolCallData struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (c *Coach) onToolCall(ev Event) {
	if rec := c.decide(ev); rec != nil {
		c.intervene(rec)
	}
}

// intervene delivers the steer. ESC first (a queued Say is read only between
// turns, useless to an agent stuck mid-loop), then the sentence (now read,
// because the loop was broken), then the training-log line. Runs OUTSIDE the
// lock — Say/Interrupt do socket IO.
//
// On the escalation trip (EscalateAfter generic steers have not worked), it hands
// off to the band-graduated agent coach ASYNCHRONOUSLY — an inference call is
// slow and must not stall the output pump the coach watches. The generic steer is
// the fallback if no agent-coach is available.
func (c *Coach) intervene(rec *SteerRecord) {
	if c.steer == nil {
		c.logSteer(*rec)
		return
	}
	if c.pol.Interrupt {
		_ = c.steer.Interrupt()
		time.Sleep(150 * time.Millisecond) // let the TUI return to its input box
	}

	c.mu.Lock()
	trip := len(c.steers) // rec is already appended, so this counts it
	doEsc := c.escalate != nil &&
		!c.escalated &&
		c.pol.EscalateAfter > 0 &&
		trip == c.pol.EscalateAfter+1
	if doEsc {
		c.escalated = true
	}
	recent := append([]string(nil), c.recent...)
	coachee := c.coachee
	escCtx := c.escCtx
	c.mu.Unlock()

	if !doEsc {
		_ = c.steer.Say(rec.Steer)
		c.logSteer(*rec)
		return
	}

	// P2b: the generic steer has failed; escalate to an agent one band up. Async,
	// so the watch loop keeps running while the coach thinks.
	if escCtx == nil {
		escCtx = context.Background()
	}
	base := *rec
	go func() {
		steer, coach, ok := c.escalate(escCtx, EscalationRequest{Coachee: coachee, Recent: recent, Trip: trip})
		if !ok || steer == "" {
			_ = c.steer.Say(base.Steer) // fall back to the generic steer
			c.logSteer(base)
			return
		}
		_ = c.steer.Say(steer)
		esc := base
		esc.Reason = "escalate"
		esc.Steer = steer
		esc.Coach = coach
		c.logSteer(esc)
	}()
}

var (
	reDigits = regexp.MustCompile(`\d+`)
	reSpace  = regexp.MustCompile(`\s+`)
	reAlnum  = regexp.MustCompile(`[a-zA-Z]`)
)

// normalizeLine turns a raw terminal line into a stable key: strip ANSI/control
// bytes, collapse whitespace, and scrub digit runs to "N" so a spinner's timer
// ("Thought for 5s") and a re-run's counter ("attempt 2") do not read as new
// content each time — which is exactly what would MASK a loop.
func normalizeLine(raw string) string {
	s := SanitizeLine(raw)
	s = reDigits.ReplaceAllString(s, "N")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// significant drops the noise that must not fill the window: blanks, decoration
// (box-drawing, spinner glyphs, pure punctuation), and lines too short to carry
// an action. Generic — no per-tool knowledge.
func significant(norm string) bool {
	if len(norm) < 8 {
		return false
	}
	return reAlnum.MatchString(norm)
}

// decide records one tool.call, updates the loop counters, and returns a
// SteerRecord when the policy trips (else nil). Pure of any session IO, so the
// signal is testable on its own.
func (c *Coach) decide(ev Event) *SteerRecord {
	var d toolCallData
	_ = json.Unmarshal(ev.Data, &d)
	key := d.Name + "|" + hashInput(d.Input)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[key]++
	c.total++
	count := c.counts[key]
	distinct := len(c.counts)
	ratio := ratioOf(c.total, distinct)

	reason := ""
	switch {
	case c.pol.RepeatThreshold > 0 && count >= c.pol.RepeatThreshold:
		reason = "repeat"
	case c.pol.RatioThreshold > 0 && c.total >= c.pol.MinCalls && ratio >= c.pol.RatioThreshold:
		reason = "ratio"
	}

	trip := reason != "" &&
		c.total >= c.pol.MinCalls &&
		len(c.steers) < c.pol.MaxSteers &&
		(len(c.steers) == 0 || distinct-c.distinctAtLast >= c.pol.Cooldown)
	if !trip {
		return nil
	}
	rec := SteerRecord{
		At: time.Now().UTC(), Reason: reason, Trigger: key, Count: count,
		Total: c.total, Distinct: distinct, Repeat: ratio,
		Steer: c.pol.Steer, Agent: c.agent,
	}
	c.steers = append(c.steers, rec)
	c.distinctAtLast = distinct
	c.noteUnresolved()
	return &rec
}

// recordPriorToolCalls preserves already-completed calls in an attached coach's
// report without feeding them into decide. Attachment starts a fresh detection
// window: past work is useful context, but can never justify a new steer.
func (c *Coach) recordPriorToolCalls(evs []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.priorCounts == nil {
		c.priorCounts = make(map[string]struct{})
	}
	for _, ev := range evs {
		if ev.Type != EventToolCall {
			continue
		}
		var d toolCallData
		_ = json.Unmarshal(ev.Data, &d)
		c.priorCounts[d.Name+"|"+hashInput(d.Input)] = struct{}{}
		c.priorTotal++
	}
}

// recordPriorPty preserves complete, significant output lines that predate an
// attached watch in its report without feeding the live novelty detector.
// Attachment starts with an empty window, recent context, and ptyLast, so past
// output can never seed or justify a new steer.
func (c *Coach) recordPriorPty(output []byte) {
	text := string(output)
	lastNewline := strings.LastIndexByte(text, '\n')
	if lastNewline < 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.priorCounts == nil {
		c.priorCounts = make(map[string]struct{})
	}
	priorLast := ""
	for _, raw := range strings.Split(text[:lastNewline], "\n") {
		norm := normalizeLine(raw)
		if !significant(norm) || norm == priorLast {
			continue
		}
		priorLast = norm
		c.priorCounts[norm] = struct{}{}
		c.priorTotal++
	}
}

// Wait blocks until the watcher goroutine has drained after the context ended.
func (c *Coach) Wait() { <-c.done }

// OutputTokens is the coach's estimate of output tokens generated (~bytes/4) —
// the spend/quota proxy.
func (c *Coach) OutputTokens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outBytes / 4
}

// Unresolved reports whether the coach exhausted its interventions (reflex +
// agent-coach) and the loop persisted — the supervisor-alert condition.
func (c *Coach) Unresolved() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pol.MaxSteers > 0 && len(c.steers) >= c.pol.MaxSteers
}

// Report summarizes the session so far.
func (c *Coach) Report() CoachReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Distinct is cumulative per mode: event mode counts tool calls, pty mode
	// counts normalized output lines. Include report-only pre-attach history in
	// either mode without feeding it into the live detector. Direct detector
	// tests may feed pty lines without first switching mode, so aggregate the
	// mutually exclusive live sets instead of selecting one by the mode label.
	all := make(map[string]struct{}, len(c.counts)+len(c.ptySeen)+len(c.priorCounts))
	for key := range c.counts {
		all[key] = struct{}{}
	}
	for key := range c.ptySeen {
		all[key] = struct{}{}
	}
	for key := range c.priorCounts {
		all[key] = struct{}{}
	}
	distinct := len(all)
	total := c.total + c.priorTotal
	return CoachReport{
		Total:    total,
		Distinct: distinct,
		Repeat:   ratioOf(total, distinct),
		Steers:   append([]SteerRecord(nil), c.steers...),
	}
}

// Mode reports the detection mode: "events" (structured tool.call stream) or
// "pty" (normalized output-line scraping). The two modes measure different
// dimensions and must never be compared directly as one metric.
func (c *Coach) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *Coach) logSteer(rec SteerRecord) {
	if c.pol.LogPath == "" {
		return
	}
	f, err := os.OpenFile(c.pol.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(rec); err == nil {
		_, _ = f.Write(append(b, '\n'))
	}
}

func hashInput(b []byte) string {
	if len(b) == 0 {
		return "none"
	}
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:6])
}

func ratioOf(total, distinct int) float64 {
	if distinct == 0 {
		return 0
	}
	return math.Round(float64(total)/float64(distinct)*100) / 100
}
