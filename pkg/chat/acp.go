package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// A REAL END OF TURN, from the tool, over a protocol.
//
// This file is rungs 1 and 2 of the ladder in pkg/agentlaunch/rung.go. Rung 4 —
// everything else in this package — decides a turn has ended by watching for 25
// seconds of SILENCE (Session.WaitIdle), which is wrong for an agent that pauses
// to think and wrong for one that renders a spinner, and costs the full 25
// seconds on the way out of every single turn. Rung 3 (events.go) fixed that for
// exactly one tool, over a side-channel file it has to be told to write.
//
// ACP fixes it over a protocol: a prompt is a REQUEST, and its response carries
// a structured StopReason. The turn is over when the agent says it is over. No
// timer, no scrape, no guess — and no pty either, so the tool's chrome never
// lands in the transcript in the first place.
//
// # What is deliberately unchanged
//
// Everything. A tool that declares no acp_exec never reaches this file, and the
// gate below means that even one that does is left alone until an operator opts
// in. Start's ACP branch is a single call at the top that reports "not mine"
// and falls through to the identical code that ran before ACP existed. The
// retrofit degrades, it never drops.

// acpDriver holds the ACP client behind a Session. Its presence on a Session is
// what makes the session an ACP session; nil means the pty transport, which is
// every session there is today.
type acpDriver struct {
	client  *acp.Client
	session string
	ctx     context.Context
	sink    io.Writer
	launch  Launch

	// end carries the completed turn. Buffered by one and drained before each
	// prompt, so a WaitIdle can never be satisfied by the PREVIOUS turn's report
	// — the exact failure the silence heuristic has, reintroduced by sloppy
	// signalling, would be a poor trade for a real protocol.
	end chan acpTurn

	mu      sync.Mutex
	turn    strings.Builder // the current turn's streamed message chunks
	thought strings.Builder // reasoning chunks not yet recorded as one event
	active  bool            // exactly one ACP prompt may be in flight

	// card and principal are the identity every timeline event carries: the
	// room card is the same session key the approval queue settles by, so an
	// event and the approval it led to name the same session.
	card      string
	principal string

	closeOnce sync.Once
}

// acpTurn is one completed prompt: what the agent said, and why it stopped.
type acpTurn struct {
	text   string
	reason acp.StopReason
	err    error
}

// startACPSession is Start's ACP branch.
//
// It reports ok=false — with no error and NOTHING done — whenever this launch
// does not belong on an ACP rung, so the caller falls through to the path it
// took before ACP existed. That "nothing done" is load-bearing: no governance,
// no budget consumed, no process spawned, no room card. A leak here would spend
// a turn's budget twice on every non-ACP launch in the fleet.
func startACPSession(ctx context.Context, agent string, opt SessionOptions) (*Session, bool, error) {
	if !agentlaunch.ACPEnabled() {
		return nil, false, nil
	}
	name, err := ResolveAgent(agent, "")
	if err != nil {
		return nil, false, nil // the ordinary path reports this identically
	}

	// The ACP launch is its OWN template, so resolve with ACP and without Steer.
	//
	// Both halves matter. A tool can speak ACP and have no interactive pty launch
	// at all, and demanding a steer_exec it does not need would refuse a session
	// we can actually drive. And a tool can speak ACP and have no HEADLESS launch
	// either — an ACP-only tool declares its {model} slot in acp_exec, so a
	// resolution that reads the headless template would report that the tool
	// "cannot select a model" and throw the binding's model away.
	//
	// Everything else about the resolution — the containerized guard, read-only
	// argv stripping, the kill-switch guard, the credential firewall's
	// PreserveEnv — is the same call, on the same options, the pty path makes.
	// An ACP transport is not a licence to hand an agent unattended full access.
	lo := opt.launchOptions()
	lo.Steer = false
	lo.ACP = true
	l, err := resolveLaunch(name, lo)
	if err != nil {
		// Not an ACP launch (no acp_exec, unknown tool, a model this tool cannot
		// take): report "not mine" and let the caller fall to the rung below,
		// exactly as it did before ACP existed.
		return nil, false, nil
	}
	catalog := newCatalog()
	if opt.Catalog != nil {
		catalog = opt.Catalog
	}
	tool, known := catalog.Tool(l.ToolName)
	if !known || !agentlaunch.EffectiveRung(tool.CLI.Launch).IsACP() {
		return nil, false, nil
	}
	// A pinned adapter is the process the ACP rung execs. One that is missing,
	// at the wrong release, or without its runtime is not driven: the tool falls
	// to the rung below, the way it did before it declared one — said out loud,
	// because an operator who set BASHY_ACP=1 expects to know why it did not
	// take.
	if tool.CLI.Launch.ACPAdapter != nil {
		st := CheckACPAdapter(tool)
		if !st.Available {
			fmt.Fprintf(os.Stderr, "chat: %s: ACP adapter %s unavailable (%s); using the next rung\n", l.Binding(), st.Spec, st.Reason)
			return nil, false, nil
		}
		if !st.Native {
			l.Tool, l.ManagedTool = st.Path, nil
		}
	}
	var preparedInbox bus.PreparedPreamble
	if strings.TrimSpace(opt.Prompt) != "" {
		preparedInbox = bus.PrepareForAgent(name, opt.Prompt)
		if err := preparedInbox.Err(); err != nil {
			return nil, true, fmt.Errorf("chat: prepare opening inbox: %w", err)
		}
		opt.Prompt = preparedInbox.Text
	}

	// From here we are committed, and errors are real errors: falling through to
	// the pty path after governing would bill the turn twice.
	next, d, err := governLaunch(ctx, name, l, opt.Prompt, lo)
	if err != nil {
		return nil, true, err
	}
	if d.Action == llmbudget.Queue {
		return nil, true, fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, l.Binding(), d.Reason)
	}
	if d.Action == llmbudget.Block {
		return nil, true, fmt.Errorf("chat: LLM budget blocked %s: %s", l.Binding(), d.Reason)
	}
	l = next

	cwd := opt.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	s := &Session{
		Agent:        l.Binding(),
		Nick:         l.Nick,
		inboxAgent:   name,
		launch:       l,
		allowPremium: opt.AllowPremium,
		done:         make(chan struct{}),
		lastWrite:    time.Now(),
	}
	sink := io.Writer(&sessionWriter{s: s})
	if opt.Stream != nil {
		sink = io.MultiWriter(sink, opt.Stream)
	}
	drv := &acpDriver{ctx: ctx, sink: sink, launch: l, end: make(chan acpTurn, 1),
		card: agentID(l), principal: principalName()}

	// Same trust preseed as the pty path: an ACP agent that stops to ask about
	// trust on stdio would deadlock the protocol rather than merely stall a TUI.
	cmd := agentCommand(withLaunch(ctx, l), l.Tool, l.Args, cwd)
	if p, ok := agentctl.ProfileFor(l.ToolName); ok && p.Preseed != "" {
		_ = agentctl.ApplyTrustPreseed(cmd.Dir, p.Preseed)
	}

	setProcessGroup(cmd)
	s.budgetCommand = cmd
	budgetWork, budgetErr := reserveBudgetWork(ctx, l, opt.Prompt, s.Agent, true, opt.AllowPremium)
	if budgetErr != nil {
		return nil, true, budgetErr
	}
	s.budgetWorks = append(s.budgetWorks, budgetWork)
	handler := newACPHandler(drv, agentPermission(name, catalog))
	client, err := acp.NewClient(ctx, handler, cmd)
	if err != nil {
		if cmd.Process == nil {
			_ = budgetWork.abort()
		} else {
			_ = budgetWork.finish("", err)
		}
		return nil, true, fmt.Errorf("chat: %s speaks ACP but would not start: %w", l.Binding(), err)
	}
	drv.client = client
	sid, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = client.Close()
		_ = budgetWork.finish("", err)
		return nil, true, fmt.Errorf("chat: %s would not open an ACP session: %w", l.Binding(), err)
	}
	drv.mu.Lock()
	drv.session = sid
	drv.mu.Unlock()
	s.acp = drv

	// Join the host room like any other member. CtlSock is empty: an ACP session
	// is steered through the protocol, not through a pty control socket, and
	// advertising a socket nobody is listening on would be worse than none.
	mode := strings.TrimSpace(opt.Mode)
	if mode == "" {
		mode = "session"
	}
	card := room.Card{
		ID:        agentID(l),
		Principal: principalName(),
		Tool:      l.ToolName,
		Model:     l.ModelName,
		Binding:   l.Binding(),
		Nick:      l.Nick,
		Band:      bindingBand(name),
		Mode:      mode,
		Task:      strings.TrimSpace(opt.Task),
		PID:       os.Getpid(),
		Cwd:       cwd,
		Events:    true, // it reports its own turn boundaries — the best kind
		Caps:      []string{room.CapInboxDelivery},
	}
	_ = room.Join(card)
	s.acpCard = card.ID

	// WATCH FOR A DEATH NOBODY ASKED FOR.
	//
	// closeACP is the caller's path out, so before this every route to
	// close(s.done) began with the caller deciding to leave. An agent that
	// crashed — or was OOM-killed, or exited on its own — closed nothing: the
	// protocol just went quiet, Live() kept answering true, and anything
	// waiting on the session waited forever. The pty path never had this hole
	// because a dead child closes the pty and the reader returns EOF; ACP has
	// no equivalent symptom, which is exactly why it needs an explicit watcher.
	//
	// Started AFTER the room join so a death cannot race past the card and skip
	// room.Leave, which would strand a member card for a process that is gone.
	// closeACP is sync.Once-guarded, so the ordinary caller path and this one
	// cannot double-close.
	if dead := client.Done(); dead != nil {
		go func() {
			<-dead
			s.closeACP()
		}()
	}

	// The opening prompt needs no waitReady and no re-send ladder. Those exist
	// because a TUI silently swallows keystrokes it is not ready for; a JSON-RPC
	// request is either answered or it is an error.
	if strings.TrimSpace(opt.Prompt) != "" {
		if err := drv.prompt(opt.Prompt); err != nil {
			s.closeACP()
			return s, true, err
		}
	}
	recordPreambleAdmission(ctx, preparedInbox)
	if err := preparedInbox.Commit(); err != nil {
		s.closeACP()
		return s, true, fmt.Errorf("chat: opening ACP prompt was delivered but its inbox acknowledgement failed: %w", err)
	}
	s.startInboxRelay(ctx)
	return s, true, nil
}

// prompt starts a turn. It does not block: Start and Say both return as soon as
// the agent has been asked, and WaitIdle is where a caller waits for the answer
// — the same shape the pty path has, so callers do not change.
func (d *acpDriver) prompt(text string) error {
	d.mu.Lock()
	if d.active {
		d.mu.Unlock()
		return fmt.Errorf("chat: ACP turn is still active")
	}
	d.active = true
	d.mu.Unlock()
	// Drop any unread report from a previous turn: this turn's boundary is the
	// only one that can end this turn.
	select {
	case <-d.end:
	default:
	}
	d.mu.Lock()
	d.turn.Reset()
	d.mu.Unlock()

	go func() {
		callCtx, endObservation := startGenAIObservation(d.ctx, d.launch)
		reason, err := d.client.Prompt(callCtx, d.session, text)
		d.flushThought()
		d.mu.Lock()
		said := d.turn.String()
		d.turn.Reset()
		d.active = false
		d.mu.Unlock()
		select {
		case d.end <- acpTurn{text: said, reason: reason, err: err}:
		default:
		}
		endGenAIObservation(endObservation, d.launch, text, said, string(reason), err)
	}()
	return nil
}

func (d *acpDriver) idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Preserve the completed turn for the caller's WaitIdle. Starting the inbox
	// turn before that result is consumed would make prompt() drain it as stale.
	return !d.active && len(d.end) == 0
}

// waitACPTurn is WaitIdle for an ACP session: it waits for the agent to SAY the
// turn ended. No ticker, no quiet period, no 25-second exit tax.
func (s *Session) waitACPTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t := <-s.acp.end:
		s.mu.Lock()
		// The answer arrives as DATA. Turn() prefers it over the buffer for the
		// same reason the events path does: nothing here was scraped.
		s.reported = t.text
		s.acpStop = t.reason
		s.mu.Unlock()
		if t.err != nil {
			return fmt.Errorf("chat: %s: ACP turn failed: %w", s.Nick, t.err)
		}
		return nil
	}
}

// ACPStopReason is why the agent stopped its last completed turn ("end_turn",
// "refusal", "cancelled", …), or "" for a session that is not on an ACP rung.
//
// This is the whole point of the retrofit, exposed: rung 4 cannot answer this
// question at all, because silence has no reason attached to it.
func (s *Session) ACPStopReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.acpStop)
}

// Rung reports which transport this session is being driven on.
func (s *Session) Rung() agentlaunch.Rung {
	if s.acp != nil {
		// Which of the two ACP rungs is a property of the tool's declaration.
		if tool, ok := newCatalog().Tool(s.launch.ToolName); ok {
			return agentlaunch.RungFor(tool.CLI.Launch)
		}
		return agentlaunch.RungACPNative
	}
	if s.events != nil {
		return agentlaunch.RungEvents
	}
	return agentlaunch.RungPTY
}

// closeACP ends the ACP session: cancel anything in flight, reap the
// subprocess, record what the conversation spent, and leave the room.
func (s *Session) closeACP() {
	s.acp.closeOnce.Do(func() {
		_ = s.acp.client.Cancel(context.Background(), s.acp.session)
		_ = s.acp.client.Close()
		s.mu.Lock()
		spoke := s.buf.String()
		s.mu.Unlock()
		s.mu.Lock()
		s.err = errors.Join(s.err, s.finishBudgetWorks(spoke, budgetCompletionError(s.budgetCommand)))
		s.mu.Unlock()
		if s.acpCard != "" {
			room.Leave(s.acpCard)
		}
		close(s.done)
	})
}

// acpHandler bridges ACP session updates into the two things a Session does
// with them. The agent's ANSWER goes to the sink, which is the same
// sessionWriter (plus the caller's optional tee) the pty path writes through,
// so callers that watch a session see an ACP agent exactly as they see a
// scraped one. Everything else the protocol reports — tool calls, plans,
// reasoning, usage, permission requests — goes to the host event log (the room
// timeline, bashy events) as structured entries, and never into the transcript:
// inventing chrome the agent did not print would corrupt the very thing ACP is
// supposed to give us, a clean answer.
type acpHandler struct {
	acp.BaseHandler
	d *acpDriver
	// approvals answers permission requests when the agent's permission mode is
	// not auto. Nil keeps BaseHandler's auto-allow — the default, unchanged.
	approvals acp.Handler
}

// newACPHandler wires a driver to the approval queue for permission mode perm.
// The approval's session is the driver's room card, so `chat grant`, `bashy
// approve` and the timeline all name the same session.
func newACPHandler(d *acpDriver, perm string) *acpHandler {
	h := &acpHandler{d: d}
	if perm != "" && perm != fleet.PermissionAuto {
		h.approvals = &acp.ApprovalHandler{
			Mode:    perm,
			Session: func() string { return d.card },
		}
	}
	return h
}

func (h *acpHandler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (acp.PermissionResponse, error) {
	var (
		resp acp.PermissionResponse
		err  error
	)
	if h.approvals != nil {
		resp, err = h.approvals.RequestPermission(ctx, req)
	} else {
		resp, err = h.BaseHandler.RequestPermission(ctx, req)
	}
	// Recorded in EVERY mode, auto included: an auto-allowed request is still a
	// request the agent made, and it is the one an operator most needs to see.
	if h.d != nil {
		h.d.recordPermission(req, resp, err)
	}
	return resp, err
}

// agentPermission is the permission mode a named agent record declares; "" for a
// bare tool or tool:model binding, which have no record.
func agentPermission(name string, catalog *fleet.Catalog) string {
	if catalog == nil {
		catalog = newCatalog()
	}
	if a, ok := catalog.Agent(name); ok {
		return strings.ToLower(strings.TrimSpace(a.Permission))
	}
	return ""
}

func (h *acpHandler) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	u := n.Update
	if u.AgentThoughtChunk == nil {
		// Reasoning streams in many small chunks; it is recorded as ONE entry
		// when something else happens (or the turn ends), so the log carries a
		// thought per step rather than a token per line.
		h.d.flushThought()
	}
	switch {
	case u.AgentMessageChunk != nil:
		text := u.AgentMessageChunk.Text
		if text == "" {
			return nil
		}
		h.d.mu.Lock()
		h.d.turn.WriteString(text)
		h.d.mu.Unlock()
		_, _ = io.WriteString(h.d.sink, text)
	case u.AgentThoughtChunk != nil:
		h.d.mu.Lock()
		h.d.thought.WriteString(u.AgentThoughtChunk.Text)
		h.d.mu.Unlock()
	case u.ToolCall != nil:
		h.d.recordToolCall(acpTopicToolCall, *u.ToolCall)
	case u.ToolCallUpdate != nil:
		h.d.recordToolCall(acpTopicToolCallUpdate, *u.ToolCallUpdate)
	case u.Plan != nil:
		h.d.recordPlan(*u.Plan)
	case u.Usage != nil:
		h.d.recordUsage(*u.Usage)
	}
	return nil
}

// THE ENTRY EVENT CONTRACT for ACP updates (Sprint 418 S3: one host log, Type
// tells domains apart, Target names the object, Topic the sub-kind, Body a short
// clipped summary — details stay with their owner).
//
//	update            Type   Topic             Target
//	tool_call         acp    tool_call         acp:<card>/tool:<id>
//	tool_call_update  acp    tool_call_update  acp:<card>/tool:<id>
//	plan              acp    plan              acp:<card>
//	thought           acp    thought           acp:<card>
//	permission        acp    permission        acp:<card>/tool:<id>
//	usage             usage  acp               acp:<card>
//
// Every entry carries Actor = the room card (the session key the approval
// queue uses), Principal = the human the session runs for, and To = the
// protocol's own session id, so an entry can be joined to both.
const (
	acpEventType           = "acp"
	acpTopicToolCall       = "tool_call"
	acpTopicToolCallUpdate = "tool_call_update"
	acpTopicPlan           = "plan"
	acpTopicThought        = "thought"
	acpTopicPermission     = "permission"
	acpUsageTopic          = "acp"
	acpEventBodyMax        = 280
)

// acpEmit appends one entry to the host log. A variable so a test can read the
// entries without parsing the timeline file.
var acpEmit = room.Emit

func (d *acpDriver) record(typ, topic, object, body string) {
	d.mu.Lock()
	session := d.session
	d.mu.Unlock()
	target := "acp:" + d.card
	if object != "" {
		target += "/" + object
	}
	// Best effort, like room.Record: the turn is the transaction, and a log
	// that could fail it would be worse than one that occasionally lags.
	_ = acpEmit(room.Event{
		Type:      typ,
		Actor:     d.card,
		Principal: d.principal,
		Target:    target,
		Topic:     topic,
		To:        session,
		Body:      room.Brief(body, acpEventBodyMax),
	})
}

func (d *acpDriver) flushThought() {
	d.mu.Lock()
	text := strings.TrimSpace(d.thought.String())
	d.thought.Reset()
	d.mu.Unlock()
	if text != "" {
		d.record(acpEventType, acpTopicThought, "", text)
	}
}

func (d *acpDriver) recordToolCall(topic string, tc acp.ToolCall) {
	parts := []string{}
	for _, f := range []string{string(tc.Status), tc.Kind, tc.Title} {
		if f = strings.TrimSpace(f); f != "" {
			parts = append(parts, f)
		}
	}
	for _, loc := range tc.Locations {
		p := loc.Path
		if loc.Line > 0 {
			p = fmt.Sprintf("%s:%d", p, loc.Line)
		}
		parts = append(parts, p)
	}
	d.record(acpEventType, topic, "tool:"+tc.ID, strings.Join(parts, " "))
}

func (d *acpDriver) recordPlan(p acp.Plan) {
	done := 0
	items := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		if e.Status == "completed" {
			done++
		}
		items = append(items, fmt.Sprintf("[%s] %s", e.Status, e.Content))
	}
	d.record(acpEventType, acpTopicPlan, "", fmt.Sprintf("%d/%d done: %s", done, len(p.Entries), strings.Join(items, "; ")))
}

func (d *acpDriver) recordUsage(u acp.Usage) {
	body := fmt.Sprintf("context %d/%d tokens", u.Used, u.Size)
	if u.CostCurrency != "" {
		body += fmt.Sprintf(", cost %.4f %s", u.CostAmount, u.CostCurrency)
	}
	d.record(room.EventUsage, acpUsageTopic, "", body)
}

// recordPermission records the TYPED request — tool kind, title, locations, the
// options offered — and what was answered, by option kind.
func (d *acpDriver) recordPermission(req acp.PermissionRequest, resp acp.PermissionResponse, err error) {
	offered := make([]string, 0, len(req.Options))
	outcome := "cancelled"
	for _, o := range req.Options {
		offered = append(offered, string(o.Kind))
		if !resp.Cancelled && o.ID == resp.OptionID {
			outcome = string(o.Kind)
		}
	}
	if err != nil {
		outcome = "error: " + err.Error()
	}
	what := strings.TrimSpace(strings.Join([]string{req.ToolCall.Kind, req.ToolCall.Title}, " "))
	for _, loc := range req.ToolCall.Locations {
		what += " " + loc.Path
	}
	d.record(acpEventType, acpTopicPermission, "tool:"+req.ToolCall.ID,
		fmt.Sprintf("%s -> %s (offered %s)", what, outcome, strings.Join(offered, ",")))
}

// ACPAdapterStatus is the availability check for a tool's pinned ACP adapter.
type ACPAdapterStatus struct {
	Tool string `json:"tool"`
	// Spec is the pinned npm spec (package@version).
	Spec string `json:"spec"`
	// Native means the adapter is the tool's own binary (gemini --acp).
	Native bool `json:"native"`
	// Path is the resolved executable, "" when not found.
	Path string `json:"path,omitempty"`
	// Installed is the version read from the installed package.json, "" when
	// the executable is not inside an npm package bashy can read.
	Installed string `json:"installed,omitempty"`
	// Verified is true when Installed equals the pin.
	Verified  bool   `json:"verified"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// CheckACPAdapter reports whether the tool's pinned adapter can be driven
// here: the pin is valid, its executable resolves (the per-adapter
// BASHY_TOOL_BINARY_<ID> override first, then PATH and the user binary homes),
// its runtime requirements are on PATH, and — when the executable lives in an
// npm package — the installed release IS the pinned one. A different release
// is unavailable, never "close enough": the digest pinned is for that release.
func CheckACPAdapter(t fleet.Tool) ACPAdapterStatus {
	a := t.CLI.Launch.ACPAdapter
	st := ACPAdapterStatus{Tool: t.Name}
	if a == nil {
		st.Reason = "no acp_adapter declared"
		return st
	}
	st.Spec = a.PackageSpec()
	st.Native = a.Bin == t.Binary()
	if err := t.ValidateACPAdapter(); err != nil {
		st.Reason = err.Error()
		return st
	}
	if p := strings.TrimSpace(os.Getenv(fleet.ToolBinaryOverrideEnv(a.ID))); p != "" {
		st.Path = p
	} else if p, err := exec.LookPath(a.Bin); err == nil {
		st.Path = p
	} else {
		st.Path = ResolveToolBinary(a.Bin)
	}
	if st.Path == "" {
		st.Reason = fmt.Sprintf("%s not found; install with: npm install -g %s", a.Bin, st.Spec)
		return st
	}
	for _, req := range a.Requires {
		if _, err := exec.LookPath(req); err != nil {
			st.Reason = fmt.Sprintf("%s needs %s on PATH", a.Bin, req)
			return st
		}
	}
	st.Installed = installedNPMVersion(st.Path, a.Package)
	if st.Installed != "" && st.Installed != a.Version {
		st.Reason = fmt.Sprintf("installed %s@%s, pinned %s", a.Package, st.Installed, a.Version)
		return st
	}
	st.Verified = st.Installed == a.Version
	st.Available = true
	return st
}

// installedNPMVersion finds the package.json named pkg that owns bin — npm
// links a bin into PATH, so the real file is resolved first — and returns its
// version, or "" when bin is not inside that package. On Windows npm writes a
// <bin>.cmd shim into the prefix directory instead of a symlink, with the
// package under <prefix>\node_modules, so that layout is read as well.
func installedNPMVersion(bin, pkg string) string {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(real)
	for i := 0; i < 6; i++ {
		if v := npmPackageVersion(dir, pkg); v != "" {
			return v
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if runtime.GOOS == "windows" {
		return npmPackageVersion(filepath.Join(filepath.Dir(real), "node_modules", filepath.FromSlash(pkg)), pkg)
	}
	return ""
}

// npmPackageVersion returns the version in dir/package.json when it names pkg.
func npmPackageVersion(dir, pkg string) string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var m struct{ Name, Version string }
	if json.Unmarshal(b, &m) == nil && m.Name == pkg {
		return m.Version
	}
	return ""
}
