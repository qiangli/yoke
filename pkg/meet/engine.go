package meet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/chat"
)

const turnGuard = "You are a participant in a planning meeting. Read the topic, agenda, and transcript, then contribute one concise, focused turn (a few sentences). Long earlier turns are shown as a head/tail PREVIEW with a `file://` link — read that file for the full text only if the preview is not enough. Do not edit files or run mutating commands — return your meeting contribution as text only."

// Context-offloading budgets (LangChain Deep Agents pattern): recent turns are
// shown as full head/tail previews; older turns collapse to a one-line reference,
// so the prompt stays bounded no matter how many rounds run (which also avoids the
// argv-size fragility some agent CLIs hit on a long raw transcript).
const (
	previewFull = 520 // inline in full at or below this many chars
	previewHead = 380 // else: first N …
	previewTail = 120 // … + last N + a file:// link
	recentTurns = 8   // this many most-recent turns get a full preview
)

// preview renders one event for the replayed context: full text if short, else a
// head/tail excerpt with a file:// link to the complete turn (read-on-demand).
func preview(e Event) string {
	t := sanitizeTurn(normalizeTurnText(e.Text))
	if len(t) <= previewFull || e.File == "" {
		return oneLine(t)
	}
	head := strings.TrimSpace(t[:previewHead])
	tail := strings.TrimSpace(t[len(t)-previewTail:])
	elided := len(t) - previewHead - previewTail
	return fmt.Sprintf("%s …[+%d chars — full: file://%s]… %s",
		oneLine(head), elided, e.File, oneLine(tail))
}

// briefRef is the collapsed form for older turns: a short lead + a file link.
//
// It normalizes transport just like preview does. A turn older than the recent
// window is the LEAST scrutinized line in the replayed context, so a legacy turn
// still holding a raw Claude/NDJSON envelope must be cleaned here too — otherwise
// the one path that skips normalization becomes the one that poisons the next
// agent's prompt with machine transport.
func briefRef(e Event) string {
	t := oneLine(sanitizeTurn(normalizeTurnText(e.Text)))
	if len(t) > 120 {
		t = t[:120] + "…"
	}
	if e.File != "" {
		return fmt.Sprintf("%s [full: file://%s]", t, e.File)
	}
	return t
}

// sanitizeTurn is chat.SanitizeTurn.
//
// It used to live here, with its own ANSI regex — and that is exactly why it had
// to move. The pty's merged-stream problem belongs to EVERY consumer of a pty
// turn (a meeting's minutes, a foreman's history, a weave attribution), but each
// had grown its own copy. Only this one ever got the private-mode CSI fix, so
// only meetings stopped recording `(B[>4m[<u78` at the tail of every claude turn.
// One implementation, in the package that produces the bytes.
func sanitizeTurn(s string) string { return chat.SanitizeTurn(s) }

func transcriptContext(events []Event) string {
	// Keep only the relevant events (agenda is already in turnPrompt).
	rel := make([]Event, 0, len(events))
	for _, e := range events {
		if e.Kind != "agenda" {
			rel = append(rel, e)
		}
	}
	if len(rel) == 0 {
		return "(no turns yet)"
	}
	var b strings.Builder
	for i, e := range rel {
		who := e.Speaker
		switch e.Kind {
		case "decision":
			who = "DECISION"
		case "action":
			who = "ACTION"
		case "ledger":
			who = "FACILITATOR"
		case "replan":
			who = "FACILITATOR (new approach)"
		}
		// Decisions/actions (short + load-bearing) and the most recent turns get a
		// full preview; older turns collapse to a one-line reference.
		recent := i >= len(rel)-recentTurns
		if e.Kind == "decision" || e.Kind == "action" || recent {
			fmt.Fprintf(&b, "%s: %s\n", who, preview(e))
		} else {
			fmt.Fprintf(&b, "%s: %s\n", who, briefRef(e))
		}
	}
	return strings.TrimSpace(b.String())
}

func turnPrompt(st *State, question string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nTopic: %s\n", turnGuard, st.Topic)
	if len(st.Agenda) > 0 {
		fmt.Fprintf(&b, "Agenda: %s\n", strings.Join(st.Agenda, " | "))
	}
	if q := strings.TrimSpace(question); q != "" {
		fmt.Fprintf(&b, "Current question: %s\n", q)
	}
	return strings.TrimSpace(b.String())
}

// turnTimeout parses the session's per-turn timeout (default 20m) so a wedged
// agent can't hang the round — no external `timeout` wrapper needed.
func turnTimeout(st *State) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(st.TurnTimeout)); err == nil && d > 0 {
		return d
	}
	return 20 * time.Minute
}

// isTimeout distinguishes a per-turn deadline from an ordinary crash. The agent
// is killed by exec's context, which surfaces as "signal: killed" rather than
// context.DeadlineExceeded, so the elapsed time is the reliable signal — with a
// slack margin because the kill races the deadline.
func isTimeout(err error, elapsed, budget time.Duration) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return budget > 0 && elapsed >= budget-2*time.Second
}

// invokeAgent runs one agent turn and classifies the outcome. The classification
// is the whole point: "opencode returned no content" is useless to an operator
// who cannot tell a timeout from a crash from a considered silence.
//
// The persist callback is what makes `spoke` mean what the live channel says it
// means: "it finished; the whole turn is now in the transcript". Before this,
// invokeAgent freed the floor and its CALLER appended the event afterwards, so
// every turn had a window in which the view claimed a durable record that did
// not exist yet — and a process that died in that window (or an append that
// failed) left a `spoke` permanently lying about the transcript. Callers hand in
// how to durably record the turn; invokeAgent runs it BEFORE closing the floor
// and downgrades the reported status if it fails. A caller with nothing to
// persist (the chair's selector turns, which are deliberately off the record)
// passes nil.
func invokeAgent(ctx context.Context, st *State, name, role, instruction, question string, runner chat.Runner, persist func(Event) (Event, error)) (Event, error) {
	events, _ := readTranscript(st.ID)
	budget := turnTimeout(st)
	start := time.Now()

	// Tee this turn onto the live channel so an observer watches the argument
	// being made rather than being told, minutes later, that it was made. It is
	// a tee: `res` is unchanged, so the RECORDED turn is byte-for-byte what it
	// would have been with nobody watching.
	//
	// A terminal is given only to a tool that can USE one — the registry says
	// which (agentctl.Profile.NeedsTerminal): claude listens mid-run and has a
	// trust prompt to clear, so it gets a pty and a control socket; codex and agy
	// declare supports_say=false, so they get a pipe.
	//
	// This is not tidiness. A pty merges stdout and stderr, so a tool's chrome —
	// codex prints a version banner and its workdir — lands inside the captured
	// answer, where a pipe keeps it out. That cost is worth paying for an agent a
	// chair can interrupt, and pure loss for one that would only sit there being
	// un-steerable and noisy.
	//
	// The other half of the contract is that print mode is DECLARED, never
	// inferred. An agent CLI that decides it is headless by sniffing whether
	// stdout is a terminal is right on a pipe and wrong on a pty — claude opened
	// its REPL and sat there. A terminal changes what an agent can be ASKED; it
	// must never change what the agent DOES.
	var sock string
	usePTY := false
	if p, ok := agentctl.ProfileFor(toolOf(name)); ok && p.NeedsTerminal() {
		usePTY = true
		sock = ctlSockPath(st, name)
	}

	// A STEERABLE meeting holds each speaker open instead, so `meet say` reaches a
	// running agent rather than a socket its one-shot never listened on. Same
	// classification below either way — a turn is a turn, however it was produced.
	if st.Steerable && runner == nil {
		if ok, _ := chat.CanSteer(name); ok {
			return sessionTurn(ctx, st, name, role, instruction, question, budget, start, persist)
		}
	}

	live := newLiveWriter(st, name, role, sock)
	// Pair the floor unconditionally. Every ordinary path below closes it
	// explicitly with a real status; this deferred close is the backstop for the
	// ones that are not ordinary — a panic in the runner, or an early return added
	// later — so a `speaking` without a `spoke` can no longer strand the floor and
	// make `meet say` address an agent that is long gone. close is once-only, so
	// the explicit close still wins.
	defer live.close(statusError)
	turnReadOnly, turnAllowUnsafe := turnAuthority()
	res, err := chat.Invoke(ctx, chat.Options{
		Agent:       name,
		Role:        role,
		Instruction: instruction,
		Context:     []string{transcriptContext(events)},
		Files:       st.Context, // every participant reviews the same source set
		Cwd:         st.Cwd,
		Timeout:     budget,
		Stream:      live,
		// The tool's structured-event side-channel gets its OWN framed sink, kept
		// apart from the stdout prose tee above. An events-reporting tool (ycode)
		// streams the SAME answer on both — prose on stdout, the text of its
		// turn.end event on the side-channel — and merging them into one partial-
		// line buffer let a torn stdout chunk splice onto a JSON envelope and leak
		// raw transport to the watcher. Separate sinks frame each source on its own.
		EventStream: live.eventStream(),
		PTY:         usePTY,
		CtlSock:     sock,

		// A seat ACTS: it runs a sprint, files the story, moves the branch. See
		// turnAuthority for what that gives up (a seat used to be read-only, and
		// the integrity argument for that has not stopped being true) and for
		// $BASHY_MEET_READONLY, which puts it back host-wide.
		ReadOnly:         turnReadOnly,
		AllowUnsafe:      turnAllowUnsafe,
		KillOnParentExit: true,
	}, runner)

	ev := classifyTurn(st, name, question, res.Output, res.ExitCode, err, time.Since(start), budget)
	// Record first, then free the floor, and say how the turn ended. A watcher
	// must be able to tell a timeout from a crash from a considered silence — the
	// same distinction the transcript preserves, which is why the status is
	// carried on both channels.
	live.close(persistThenStatus(ev, persist))
	return ev, err
}

// persistThenStatus durably records the turn and reports the status the live
// channel should publish.
//
// The status it returns is about the RECORD, not only about the agent: when the
// append fails, `spoke` says statusUnrecorded rather than "ok", because the one
// thing `spoke` promises a reader is that the transcript now contains this turn.
// Reporting the agent's own status there would be a claim about durable state
// that nobody checked — the failure mode that makes a transcript gap look like
// an agent that never spoke.
//
// It returns the event the caller actually RECORDED, not the one classifyTurn
// produced, because several callers finish the event before storing it — a poll
// turns an off-menu answer into `invalid`, an optional ask turns silence into
// `abstain`. Publishing the pre-finalization status would make the live channel
// and the transcript disagree about the same turn.
func persistThenStatus(ev Event, persist func(Event) (Event, error)) string {
	if persist == nil {
		return ev.Status
	}
	recorded, err := persist(ev)
	if err != nil {
		return statusUnrecorded
	}
	if recorded.Status != "" {
		return recorded.Status
	}
	return ev.Status
}

// classifyTurn turns "what came back" into a transcript event.
//
// It is shared by BOTH turn paths — the headless one-shot and the live steerable
// session — because how a turn was produced must not change how it is judged. A
// meeting whose minutes said "opencode returned no content" in one mode and
// "opencode timed out" in the other, for the same underlying silence, would be
// reporting on its own plumbing rather than on the discussion.
func classifyTurn(st *State, name, question, out string, exit int, err error, elapsed, budget time.Duration) Event {
	ev := Event{
		Round: st.Round, Speaker: name, Role: string(RoleParticipant), Kind: "turn",
		Question: question, TS: nowFn(),
		ExitCode: exit, DurMS: elapsed.Milliseconds(),
	}
	switch {
	case err != nil && isTimeout(err, elapsed, budget):
		ev.Status = statusTimeout
		ev.Text = fmt.Sprintf("(%s timed out after %s)", name, budget)
	case err != nil:
		// A failed turn is recorded as a SHORT marker — never the raw error
		// output (agent CLI banners/tracebacks would pollute the transcript and,
		// replayed as context, crash the next agent). It is not offloaded.
		ev.Status = statusError
		ev.Text = fmt.Sprintf("(%s unavailable this turn: %s)", name, oneLine(sanitizeTurn(shortErr(out, err))))
	default:
		// Extract the assistant's words from any structured transport (Claude
		// stream-json, first-party NDJSON) BEFORE sanitizing, so the record holds
		// prose rather than the machine envelope the CLI streamed. Plain output is
		// passed through untouched.
		text := sanitizeTurn(normalizeTurnText(out))
		ev.Chars = len(text)
		switch {
		case text == "":
			ev.Status = statusEmpty
			ev.Text = fmt.Sprintf("(%s returned no content)", name)
		case st.MinTurnChars > 0 && len(text) < st.MinTurnChars:
			ev.Status = statusShort
			ev.Text = text
			ev.File = writeTurnFile(st.ID, ev)
		default:
			ev.Status = statusOK
			ev.Text = text
			ev.File = writeTurnFile(st.ID, ev) // offload full text for read-on-demand
		}
	}
	return ev
}

// runTurn invokes one participant and appends its turn to the transcript.
//
// The append is handed DOWN as invokeAgent's persist callback rather than done
// here afterwards, so the transcript entry lands before the live channel says it
// did. The append error is captured out of the closure so the caller's contract
// is unchanged: a failed append is still returned in preference to the turn's
// own error.
func runTurn(ctx context.Context, st *State, name, question string, runner chat.Runner) (Event, error) {
	var aerr error
	ev, err := invokeAgent(ctx, st, name, "", turnPrompt(st, question), question, runner,
		func(e Event) (Event, error) {
			aerr = appendEvent(st.ID, e)
			return e, aerr
		})
	if aerr != nil {
		return ev, aerr
	}
	return ev, err
}

// shortErr produces a compact failure reason from an agent's output+error,
// bounded so a multi-KB CLI banner never enters the transcript.
//
// A FAILED turn's stdout is still transport: a Claude/ycode turn that streamed
// some words and then errored leaves stream-json / NDJSON envelopes in `out`. So
// it is normalized to the assistant's prose BEFORE sanitizing — exactly as the
// success path does — otherwise the raw envelope (session_id, {"type":"system"…},
// turn.start/tool.call) becomes the failure marker and, recorded, poisons the
// transcript, the minutes, the replayed context, and the default/--json views.
// When the transport carried no human words at all, the normalized text is empty
// and the caller falls back to the error string, never the raw stream.
func shortErr(out string, err error) string {
	s := strings.TrimSpace(sanitizeTurn(normalizeTurnText(out)))
	if s == "" {
		s = err.Error()
	}
	s = oneLine(s)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// runRound runs one sequential round across all participants.
//
// It returns an error only for a refused lease — the per-turn outcomes are
// carried on the events, as they always have been, because a participant that
// failed is a fact about the meeting rather than a failure of running it.
func runRound(ctx context.Context, st *State, question string, runner chat.Runner) ([]Event, error) {
	// A board never has its floor run for it — no chair, no orchestrated turns.
	// Belt-and-braces with the api.Round/deliberate guards so no caller can spawn
	// a round-robin over a board's participants.
	if st.board() {
		return nil, st.boardRefusal("run a round")
	}
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	// Bumped under the lease. This increment plus the turn loop below is the
	// read-modify-write two concurrent runners used to interleave.
	st.Round++
	_ = st.save()
	out := make([]Event, 0, len(st.Participants))
	for _, name := range st.Participants {
		ev, _ := runTurn(ctx, st, name, question, runner)
		out = append(out, ev)
	}
	return out, nil
}

// record appends a marker/human event. Human/marker text is sanitized too — it
// is replayed as prompt context to agents, so stray control/invalid-UTF-8 bytes
// there would crash the same downstream tools as raw agent banners.
func record(st *State, kind, speaker, role, text string) (Event, error) {
	ev := Event{Round: st.Round, Speaker: speaker, Role: role, Kind: kind, Text: sanitizeTurn(text), TS: nowFn()}
	return ev, appendEvent(st.ID, ev)
}

func currentAgenda(st *State) string {
	if st.Round > 0 && st.Round <= len(st.Agenda) {
		return st.Agenda[st.Round-1]
	}
	if len(st.Agenda) > 0 {
		return st.Agenda[0]
	}
	return ""
}

func findRepoRoot(dir string) string {
	d := dir
	for d != "" {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return ""
}

// minutesPath resolves where the published minutes go. An explicit --out wins;
// otherwise docs/meetings/ inside a git repo, else the session store.
//
// --out MAY BE A DIRECTORY, and this used to break. The flag's own help says
// "docs | kb | <path>", which invites you to pass a directory — and the path was
// then handed straight to atomicWrite, which writes `<path>.tmp` and renames it
// onto `<path>`. Renaming a file onto an existing DIRECTORY fails, so the whole
// meeting died at the last step with:
//
//	rename /…/scratchpad.tmp /…/scratchpad: file exists
//
// after every participant had already spoken and been paid for. The minutes were
// lost at the moment they were finished.
//
// So: a directory gets the generated filename INSIDE it. Anything else is taken
// as the file itself.
func minutesPath(st *State) string {
	out := strings.TrimSpace(st.Out)
	ts := st.Created.Format("2006-01-02T15-04")
	name := fmt.Sprintf("meeting-note-%s-%s.md", ts, slugify(st.Topic))

	// OutStore keeps the minutes out of the repo entirely — see its doc comment.
	// Checked before the repo default, because the whole point is that being
	// inside a git repo must NOT pull these into the working tree.
	if out == OutStore {
		dir, _ := storeDir(st.ID)
		return filepath.Join(dir, "minutes.md")
	}
	if out != "" && out != "docs" && out != "kb" {
		if fi, err := os.Stat(out); err == nil && fi.IsDir() {
			return filepath.Join(out, name)
		}
		return out
	}
	if root := findRepoRoot(st.Cwd); root != "" {
		return filepath.Join(root, "docs", "meetings", name)
	}
	dir, _ := storeDir(st.ID)
	return filepath.Join(dir, "minutes.md")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// minutesTurnChars bounds one turn's full text in the published minutes. The
// per-turn file always holds the complete bytes, so nothing is lost — but the
// minutes must carry the ARGUMENT, not a 240-char ellipsis of it.
const minutesTurnChars = 4000

// blockquote renders a turn's full text as a markdown blockquote, capped, with a
// pointer to the complete per-turn file when it was truncated.
func blockquote(text, file string) string {
	t := strings.TrimSpace(text)
	truncated := false
	if len(t) > minutesTurnChars {
		t = strings.TrimSpace(t[:minutesTurnChars])
		truncated = true
	}
	var b strings.Builder
	for line := range strings.SplitSeq(t, "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}
	if truncated {
		if file != "" {
			fmt.Fprintf(&b, ">\n> *(truncated — full text: `%s`)*\n", redactHome(file))
		} else {
			b.WriteString(">\n> *(truncated)*\n")
		}
	}
	return b.String()
}

// renderMinutes builds the deterministic minutes document.
//
// Decisions and action items come from explicit human markers in the transcript
// PLUS the secretary's synthesis; inferred decisions are labelled so a reader can
// always tell what was stated from what was read out of a consensus.
// noSynthesisNotice is what the minutes say when the secretary pass never
// completed. It must never read like a finding: the transcript may well hold
// decisions that nothing has extracted yet. `meet amend --resynthesize
// [--secretary AGENT]` is the recovery.
const noSynthesisNotice = "**UNKNOWN — the secretary pass did not complete, so nothing was extracted.** " +
	"This is NOT a finding that the meeting decided nothing; read the turns below. " +
	"Recover with `bashy meet amend <id> --resynthesize [--secretary AGENT]`.\n"

// noSecretaryNotice is the same honesty for a room that never had a secretary.
// It has to READ differently from noSynthesisNotice: nothing failed here, and
// telling a reader to recover from a pass that was never meant to run would send
// them looking for a fault that does not exist. Both say the one true thing —
// nothing extracted this — and differ only on why.
const noSecretaryNotice = "**NOT EXTRACTED — this room has no secretary, so nothing read the discussion for decisions.** " +
	"This is NOT a finding that the room decided nothing; read the turns below. " +
	"To extract them now, name a recorder: `bashy meet amend <id> --secretary AGENT`.\n"

// unextracted returns the notice that fits why nothing was extracted.
func unextracted(st *State) string {
	if st.recorded() {
		return noSynthesisNotice
	}
	return noSecretaryNotice
}

func renderMinutes(st *State, events []Event, syn *Synthesis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Meeting — %s\n", st.Topic)
	fmt.Fprintf(&b, "Date: %s  ·  Session: `%s`\n", st.Created.Format("2006-01-02 15:04"), st.ID)
	fmt.Fprintf(&b, "Initiator: %s\n", st.initiatorLabel())

	attendees := []string{st.Human + " (human)"}
	for _, p := range st.Participants {
		attendees = append(attendees, p+" (participant)")
	}
	if st.chaired() {
		attendees = append(attendees, st.chair()+" (facilitator)")
	}
	if st.recorded() {
		attendees = append(attendees, st.secretary()+" (secretary)")
	}
	fmt.Fprintf(&b, "Attendees: %s\n", strings.Join(attendees, " · "))
	if len(st.Context) > 0 {
		fmt.Fprintf(&b, "Context reviewed: %s\n", strings.Join(st.Context, " · "))
	}
	b.WriteString("\n")
	if len(st.Agenda) > 0 {
		fmt.Fprintf(&b, "Agenda: %s\n\n", strings.Join(st.Agenda, " · "))
	}

	if syn != nil && strings.TrimSpace(syn.Summary) != "" {
		fmt.Fprintf(&b, "## Summary\n%s\n\n", redactHome(syn.Summary))
	}

	// Explicit human markers first — they are authoritative and never inferred.
	var decisions []Decision
	var actions []string
	for _, e := range events {
		switch e.Kind {
		case "decision":
			decisions = append(decisions, Decision{Text: e.Text})
		case "action":
			actions = append(actions, e.Text)
		}
	}
	if syn != nil {
		decisions = append(decisions, syn.Decisions...)
		actions = append(actions, syn.Actions...)
	}

	b.WriteString("## Decisions\n")
	switch {
	case len(decisions) == 0 && syn == nil:
		// Nothing extracted the decisions — either the secretary pass failed or
		// the room has no secretary at all. "No decision" would be a
		// success-shaped claim reached by the ABSENCE of evidence, the exact
		// shape the fleet-evidence invariant forbids. Say what is actually
		// known: nothing.
		b.WriteString(unextracted(st))
	case len(decisions) == 0:
		b.WriteString("(none — the meeting reached no decision)\n")
	default:
		for i, d := range decisions {
			tag := ""
			if d.Inferred {
				tag = " *(inferred from consensus"
				if len(d.Support) > 0 {
					tag += "; agreed: " + strings.Join(d.Support, ", ")
				}
				tag += ")*"
			}
			fmt.Fprintf(&b, "%d. %s%s\n", i+1, redactHome(d.Text), tag)
		}
	}

	b.WriteString("\n## Action items\n")
	switch {
	case len(actions) == 0 && syn == nil:
		b.WriteString(unextracted(st))
	case len(actions) == 0:
		b.WriteString("(none)\n")
	default:
		for _, a := range actions {
			fmt.Fprintf(&b, "- [ ] %s\n", redactHome(a))
		}
	}

	if syn != nil {
		writeList(&b, "Risks", syn.Risks)
		writeList(&b, "Open questions", syn.OpenQuestions)
		writeList(&b, "Corrections / revised framing", syn.Corrections)
	}

	renderPolls(&b, events)

	b.WriteString("\n## Participant coverage\n\n")
	writeCoverageTable(&b, coverage(st, events))

	b.WriteString("\n## Notes (turns)\n")
	for _, e := range events {
		switch e.Kind {
		case "human":
			fmt.Fprintf(&b, "\n**%s** (human):\n\n%s", e.Speaker, blockquote(redactHome(e.Text), ""))
		case "turn":
			status := statusOf(e)
			label := fmt.Sprintf("\n**%s** (round %d", e.Speaker, e.Round)
			if status != statusOK {
				label += ", " + status
			}
			label += "):\n\n"
			b.WriteString(label)
			b.WriteString(blockquote(redactHome(normalizeTurnText(e.Text)), e.File))
		case "confirm":
			fmt.Fprintf(&b, "\n**%s** (conclusion confirmed): %s\n", e.Speaker, oneLine(redactHome(e.Text)))
		case "ledger":
			fmt.Fprintf(&b, "\n*facilitator: %s*\n", oneLine(redactHome(e.Text)))
		case "replan":
			fmt.Fprintf(&b, "\n**%s** (facilitator re-plan after a stall):\n\n%s", e.Speaker, blockquote(redactHome(normalizeTurnText(e.Text)), e.File))
		case "note":
			fmt.Fprintf(&b, "\n*%s*\n", oneLine(redactHome(e.Text)))
		case "invite", "kick":
			// A roster change belongs in the record for the same reason a turn
			// does: a reader who sees an agent answer from round 4 onward must
			// be able to tell "it joined then" from "it failed rounds 1-3".
			fmt.Fprintf(&b, "\n*roster — %s %s*\n", e.Speaker, oneLine(redactHome(e.Text)))
		}
	}

	dir, _ := storeDir(st.ID)
	fmt.Fprintf(&b, "\nTranscript: `%s`\n", redactHome(filepath.Join(dir, "transcript.jsonl")))
	return b.String()
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n", title)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", redactHome(it))
	}
}

// turnFailed reports whether a turn event failed to contribute. An abstention on
// an optional question is NOT a failure.
func turnFailed(e Event) bool { return !contributed(e) }

// recordOperability folds each participant's per-turn outcome into the capability
// matrix's operability column (the self-updating loop). Operability is
// tool-governed, so a clean turn is a pass and a failure marker is a fail for that
// tool — a flaky agent nets down over the meeting.
func recordOperability(st *State, events []Event) {
	seat := map[string]bool{}
	for _, p := range st.Participants {
		seat[p] = true
	}
	for _, e := range events {
		if e.Kind != "turn" && e.Kind != "vote" {
			continue
		}
		if !seat[e.Speaker] {
			continue
		}
		// An abstention on an optional question is a clean turn: the tool ran and
		// the agent chose to say nothing. Only real failures net down.
		_ = capability.RecordOperability(e.Speaker, !turnFailed(e))
	}
}

// convergeInstruction builds the secretary's prompt. The decision mode is the
// load-bearing knob: `explicit` extracts only decisions a participant stated
// outright, `infer` additionally lets the secretary name a decision the meeting
// clearly converged on — which is what stops a meeting that plainly agreed on
// four things from filing "Decisions: none recorded".
//
// The guard against hallucinated consensus is not the mode, it is the label: an
// inferred decision is marked as such, and inference is still forbidden from
// inventing a position no participant took.
func convergeInstruction(mode string) string {
	var b strings.Builder
	b.WriteString("You are the meeting secretary (notes only). Read the transcript and report what happened. " +
		"You do not participate, propose, vote, or decide.\n\n")
	if mode == "explicit" {
		b.WriteString("DECISIONS: list ONLY decisions a participant stated outright as decided. " +
			"If the meeting converged on something but nobody declared it a decision, it is an OPEN QUESTION, not a decision.\n\n")
	} else {
		b.WriteString("DECISIONS: list decisions a participant stated outright, AND decisions the meeting clearly " +
			"converged on. A convergence needs BOTH a proposal and an acceptance: one participant proposed it and at " +
			"least one other AGREED, and nobody dissented. Prefix every converged-but-undeclared item with the literal " +
			"token (inferred) and end it with the literal token [agreed: name1, name2] naming the participants who " +
			"proposed and agreed. Discussing an option is NOT agreeing to it. Never invent a position no participant " +
			"actually took — if you are unsure whether it was agreed, it is an OPEN QUESTION, not a decision.\n\n")
	}
	b.WriteString("Output EXACTLY these sections, each as '- ' bullet lines, writing 'none' when empty:\n" +
		"DECISIONS:\nACTIONS:\nRISKS:\nOPEN QUESTIONS:\nCORRECTIONS:\nSUMMARY:\n\n" +
		"ACTIONS name an owner when one was stated. RISKS are hazards a participant raised. " +
		"CORRECTIONS are claims in the topic, agenda, or earlier framing that the meeting corrected or superseded — " +
		"so a reader does not mistake stale framing for an endorsed premise. " +
		"SUMMARY is 2-4 neutral sentences.")
	return b.String()
}

// converge runs the secretary's synthesis pass and persists the result to
// synthesis.json (latest pass wins). Safe to re-run: it never appends markers to
// the transcript, so `meet amend` cannot duplicate anything.
// overrideSecretary swaps in a replacement secretary and re-validates, so a
// meeting whose secretary could not launch is recoverable without re-running
// the whole deliberation. The role invariants still apply: the replacement may
// not be a participant or the chair, or the record would gain a stake in what
// it records.
func overrideSecretary(st *State, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	prev := st.Secretary
	st.Secretary = name
	if err := st.Validate(); err != nil {
		st.Secretary = prev
		return err
	}
	return st.save()
}

func converge(ctx context.Context, st *State, runner chat.Runner) (*Synthesis, error) {
	if !st.recorded() {
		return nil, fmt.Errorf("meet: %s has no secretary, so there is nothing to run a synthesis pass with — "+
			"name one with `bashy meet amend %s --secretary AGENT`", st.ID, st.ID)
	}
	events, _ := readTranscript(st.ID)
	secretaryReadOnly, secretaryAllowUnsafe := turnAuthority()
	res, err := chat.Invoke(ctx, chat.Options{
		Agent: st.Secretary, Role: string(RoleSecretary), Instruction: convergeInstruction(st.decisionMode()),
		Context: []string{transcriptContext(events)}, Cwd: st.Cwd, Timeout: turnTimeout(st), KillOnParentExit: true,
		// The secretary writes the minutes, and under a manager seat it also
		// files what the room decided. Same authority as every other seat — see
		// turnAuthority.
		ReadOnly: secretaryReadOnly, AllowUnsafe: secretaryAllowUnsafe,
	}, runner)
	if err != nil {
		return nil, fmt.Errorf("meet: secretary %s failed: %w", st.Secretary, err)
	}
	syn := parseConverge(sanitizeTurn(res.Output))
	syn.demoteUnsupported() // an inferred decision with no named acceptance is an open question
	syn.By, syn.At, syn.Mode = st.Secretary, nowFn(), st.decisionMode()
	if err := syn.save(st.ID); err != nil {
		return nil, err
	}
	return syn, nil
}

// supportTag matches the trailing `[agreed: codex, claude]` grounding token.
var supportTag = regexp.MustCompile(`(?i)\[\s*agreed\s*:\s*([^\]]*)\]`)

// parseDecision pulls the `(inferred)` prefix and the `[agreed: …]` support list
// off one decision line.
func parseDecision(item string) Decision {
	d := Decision{Text: item}
	if m := supportTag.FindStringSubmatch(d.Text); m != nil {
		for _, name := range strings.Split(m[1], ",") {
			if n := strings.TrimSpace(name); n != "" {
				d.Support = append(d.Support, n)
			}
		}
		d.Text = strings.TrimSpace(supportTag.ReplaceAllString(d.Text, ""))
	}
	if rest, ok := cutPrefixFold(d.Text, "(inferred)"); ok {
		d.Inferred = true
		d.Text = strings.TrimSpace(rest)
	}
	d.Text = strings.TrimSpace(strings.Trim(d.Text, "—- "))
	return d
}

// cutPrefixFold is strings.CutPrefix, case-insensitive.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// parseConverge splits the secretary's labeled output into a Synthesis. An
// unsectioned reply degrades to a summary rather than being dropped.
func parseConverge(s string) *Synthesis {
	syn := &Synthesis{}
	cur := ""
	var sumParts []string
	for line := range strings.SplitSeq(s, "\n") {
		l := strings.TrimSpace(line)
		switch up := strings.ToUpper(l); {
		case strings.HasPrefix(up, "DECISION"):
			cur = "d"
			continue
		case strings.HasPrefix(up, "ACTION"):
			cur = "a"
			continue
		case strings.HasPrefix(up, "RISK"):
			cur = "r"
			continue
		case strings.HasPrefix(up, "OPEN QUESTION"):
			cur = "q"
			continue
		case strings.HasPrefix(up, "CORRECTION"):
			cur = "c"
			continue
		case strings.HasPrefix(up, "SUMMARY"):
			cur = "s"
			continue
		}
		if l == "" {
			continue
		}
		item := strings.TrimSpace(strings.TrimLeft(l, "-*•0123456789. "))
		if item == "" || strings.EqualFold(item, "none") {
			continue
		}
		switch cur {
		case "d":
			syn.Decisions = append(syn.Decisions, parseDecision(item))
		case "a":
			syn.Actions = append(syn.Actions, item)
		case "r":
			syn.Risks = append(syn.Risks, item)
		case "q":
			syn.OpenQuestions = append(syn.OpenQuestions, item)
		case "c":
			syn.Corrections = append(syn.Corrections, item)
		case "s":
			sumParts = append(sumParts, item)
		}
	}
	syn.Summary = strings.TrimSpace(strings.Join(sumParts, " "))
	if syn.Summary == "" && len(syn.Decisions)+len(syn.Actions)+len(syn.Risks)+len(syn.OpenQuestions)+len(syn.Corrections) == 0 {
		syn.Summary = strings.TrimSpace(s) // unsectioned reply → treat as the summary
	}
	return syn
}

// ErrDeclined is returned when the initiator refuses to conclude the meeting.
var ErrDeclined = errors.New("meet: initiator declined to conclude the meeting")

// isTerminal reports whether r is an interactive terminal we may prompt on.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmConclusion asks the meeting's INITIATOR whether it may end, and records
// the answer. A human initiator is prompted on the terminal; an agent initiator
// is asked through the secretary's own channel, which is what lets an agent
// convene a meeting as a tool call and stay in control of when it ends.
//
// `yes` is the operator's explicit unattended override; it is recorded, not
// silent.
func confirmConclusion(ctx context.Context, st *State, in io.Reader, out io.Writer, yes bool, runner chat.Runner) error {
	who, kind := st.initiatorName(), st.initiatorKind()
	if yes {
		if who == "" {
			who = "unnamed caller"
		}
		_, _ = record(st, "confirm", who, kind, "concluded without prompting (--yes)")
		return nil
	}
	// An unnamed initiator is the agent that called `meet consult`, which receives
	// the verdict synchronously and never confirms. Reaching here means someone
	// asked an anonymous caller to confirm; there is nobody to ask.
	if who == "" {
		return fmt.Errorf("meet: this meeting has no named initiator to confirm it may end; " +
			"pass --initiator <name at the table>, or --yes for an unattended close")
	}

	events, _ := readTranscript(st.ID)
	syn := loadSynthesis(st.ID)
	var dec, act int
	if syn != nil {
		dec, act = len(syn.Decisions), len(syn.Actions)
	}
	brief := fmt.Sprintf("%d rounds, %d turns, %d decisions, %d action items", st.Round, countKind(events, "turn"), dec, act)

	// A board never spawns a participant, not even to confirm its own conclusion:
	// there is no turn to ask them on. An agent initiator of a board falls through
	// to the attended path below, which concludes only with a terminal answer or an
	// explicit --yes — never by launching the agent.
	if kind == "agent" && !st.board() {
		// Only claim a synthesis exists when one does — a room with no secretary
		// has none, and pointing the initiator at it would send it looking for
		// something that was never written.
		where := "The transcript is below."
		if syn != nil {
			where = "The secretary's synthesis is in the transcript below."
		}
		instr := fmt.Sprintf(
			"You convened this meeting (topic: %q). It has run %s. %s\n\n"+
				"Has the meeting achieved what you convened it for? Reply on the FIRST line with EXACTLY one word: "+
				"CONCLUDE or CONTINUE. On the next line give one sentence of reason.", st.Topic, brief, where)
		// nil persist: the initiator's CONCLUDE/CONTINUE reply is not a transcript
		// turn — what gets recorded below is the derived `confirm` event.
		ev, err := invokeAgent(ctx, st, who, "", instr, "conclude?", runner, nil)
		if err != nil {
			return fmt.Errorf("meet: could not reach initiator %s to confirm conclusion: %w", who, err)
		}
		verdict, reason := parseVerdict(ev.Text)
		_, _ = record(st, "confirm", who, "agent", fmt.Sprintf("%s — %s", verdict, reason))
		if verdict != "CONCLUDE" {
			fmt.Fprintf(out, "meet: initiator %s asked to CONTINUE: %s\n", who, reason)
			return ErrDeclined
		}
		return nil
	}

	if !isTerminal(in) {
		return fmt.Errorf("meet: %s must confirm the meeting may end, but stdin is not a terminal; pass --yes for an unattended close", who)
	}
	fmt.Fprintf(out, "\nmeet: %s — %s\nEnd the meeting and file the minutes? [y/N] ", st.ID, brief)
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return fmt.Errorf("meet: no confirmation received; pass --yes for an unattended close")
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		_, _ = record(st, "confirm", who, "human", "confirmed at the prompt")
		return nil
	}
	return ErrDeclined
}

// parseVerdict pulls CONCLUDE/CONTINUE plus a reason out of an agent's reply.
// Defaults to CONTINUE: an unparseable answer must not end someone's meeting.
func parseVerdict(text string) (verdict, reason string) {
	verdict = "CONTINUE"
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, l := range lines {
		up := strings.ToUpper(l)
		switch {
		case strings.Contains(up, "CONCLUDE"):
			verdict = "CONCLUDE"
		case strings.Contains(up, "CONTINUE"):
			verdict = "CONTINUE"
		default:
			continue
		}
		reason = strings.TrimSpace(strings.Join(lines[i+1:], " "))
		if reason == "" {
			reason = oneLine(text)
		}
		return verdict, oneLine(reason)
	}
	return verdict, "no CONCLUDE/CONTINUE verdict in the reply — defaulting to CONTINUE"
}

func countKind(events []Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// closeOptions controls the close path. Synthesize runs the secretary; Confirm
// gates the close on the initiator's agreement.
type closeOptions struct {
	Synthesize bool
	Confirm    bool
	Yes        bool
	In         io.Reader
	Out        io.Writer
}

// closeMeeting converges, asks the initiator to confirm, then writes the minutes.
func closeMeeting(ctx context.Context, st *State, opt closeOptions, runner chat.Runner) (string, error) {
	if st.Status == "closed" {
		return minutesPath(st), nil
	}
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	// A room with no secretary has no pass to run. Skipping it silently is the
	// point: warning that the "secretary pass failed" would report a fault for a
	// room that was never asked to keep minutes.
	if opt.Synthesize && st.recorded() {
		if _, err := converge(ctx, st, runner); err != nil {
			fmt.Fprintf(opt.Out, "meet: ⚠ secretary pass failed, filing without a synthesis: %v\n", err)
		}
	}
	if opt.Confirm {
		if err := confirmConclusion(ctx, st, opt.In, opt.Out, opt.Yes, runner); err != nil {
			return "", err
		}
	}
	path, err := fileMinutes(st)
	if err != nil {
		return "", err
	}
	// A board that absorbs a thread and never reports back is how the board
	// becomes a place nobody trusts. On close, post the outcome to mb WITH the
	// room id so the correlation the board opened is closed on the same board.
	if st.board() {
		if reason := postBoardOutcome(st); reason != "" {
			fmt.Fprintf(opt.Out, "meet: ⚠ board outcome not posted to mb: %s\n", reason)
		}
	}
	return path, nil
}

// postBoardOutcome announces a closed board's result on mb, keyed by the room id.
// It returns a human reason when it could not post (no seam wired, or the post
// failed) and "" on success or when there was nothing to report; closeMeeting
// surfaces the reason rather than failing the close, because a filed room that
// could not announce itself is still closed.
func postBoardOutcome(st *State) string {
	if PostMB == nil {
		return "no message-board seam is wired"
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		return err.Error()
	}
	msgs := 0
	for _, e := range events {
		if e.Kind == "message" {
			msgs++
		}
	}
	body := fmt.Sprintf("board closed — %q, %d message(s) from %d seat(s). Room id: %s",
		st.Topic, msgs, len(st.Participants), st.ID)
	if _, err := PostMB(MBPost{From: st.initiatorName(), Topic: st.Topic, Body: body}, nil); err != nil {
		return err.Error()
	}
	return ""
}

// fileMinutes renders and writes the minutes from whatever is on disk. It is the
// shared tail of `close` and `amend`.
//
// Two things it deliberately does NOT do. First, it will not file a document for
// a room that never held a discussion: a transcript with no turn, vote, post, or
// marker produces only NOT-EXTRACTED boilerplate, so closing such a room releases
// its number and marks it closed but writes nothing — the transcript stays in the
// store. Second, it will not write into a repository that is not the caller's.
// minutesPath() derives the repo-default destination from st.Cwd, captured when
// the room was OPENED; a room opened in one repo and closed weeks later from
// another would otherwise drop a note into the first, silently dirtying a tree
// nobody is standing in (measured: a close landed a note in a submodule mid
// certification, from a room three weeks stale). When the recorded Cwd names a
// different repo than the caller's, it refuses and names the path.
func fileMinutes(st *State) (string, error) {
	events, err := readTranscript(st.ID)
	if err != nil {
		return "", err
	}
	recordOperability(st, events) // self-update the capability matrix from this meeting

	// Did anything actually happen in this room? Agenda/procedural markers are
	// scaffolding, not a discussion — a room holding only those has nothing worth
	// filing. Boards post as "human", so a board with activity is caught here too.
	filable := false
	for _, e := range events {
		switch e.Kind {
		case "turn", "vote", "human", "decision", "action", "note":
			filable = true
		}
	}
	if !filable {
		// Still close the room and release its number; just publish nothing. The
		// transcript remains in the store for anyone who wants it.
		st.Status = "closed"
		st.Room = 0
		_ = st.save()
		dir, _ := storeDir(st.ID)
		return dir, nil
	}

	path := minutesPath(st)
	// Resolve the destination against the caller's repo at CLOSE time. If the
	// minutes would land inside the repo the room was opened in, and that is not
	// the repo the caller is standing in now, refuse rather than write into it.
	if recordedRoot := findRepoRoot(st.Cwd); recordedRoot != "" && strings.HasPrefix(path, recordedRoot) {
		cwd, _ := os.Getwd()
		if callerRoot := findRepoRoot(cwd); !sameRepoDir(recordedRoot, callerRoot) {
			return "", fmt.Errorf("meet: refusing to file minutes into %s — the room was opened in %s, "+
				"but you are closing it from %s. The transcript remains in the store; re-run from that "+
				"repo, or pass --out <path> to choose a destination",
				redactHome(path), redactHome(recordedRoot), redactHome(cwd))
		}
	}

	md := renderMinutes(st, events, loadSynthesis(st.ID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := atomicWrite(path, []byte(md)); err != nil {
		return "", err
	}
	st.Status = "closed"
	st.Room = 0
	_ = st.save()
	return path, nil
}

// sameRepoDir reports whether two repo roots are the same directory, resolving
// symlinks first. macOS tempdirs are /var → /private/var symlinks, so a literal
// string compare between a recorded Cwd and os.Getwd() can disagree about the
// same place; EvalSymlinks makes the comparison canonical.
func sameRepoDir(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}
