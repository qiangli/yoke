package meet

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/chat"
)

// The CHAIR: the role that decides process.
//
// When an agent facilitates (`--owner <agent>`), it directs the discussion. When no
// agent chairs, the meeting is a fixed round-robin and nobody directs. That is
// the entire turn-model question — there is no separate mode.
//
// Round-robin is rigid: it cannot skip a participant with nothing to add, cannot
// call the same one twice when it is mid-argument, and — the reason this file
// exists — cannot notice that the meeting is going in circles. Step repetition is
// the single largest measured failure mode in multi-agent systems.
//
// So the chair does not merely answer "who speaks next." Every turn it rebuilds a
// structured PROGRESS LEDGER answering five questions at once, and speaker
// selection is two of the five. The orchestrator — this Go code, not the model —
// owns termination, applies the backstops, and validates the selection.
//
// The chair never argues and never records. Both are enforced by State.Validate:
// a chair that participates would pick itself, and a chair that also kept the
// minutes could conclude the meeting and then author the record of it.
//
// Three defenses, each drawn from a documented failure:
//
//  1. SELECTOR VALIDATION LADDER. An LLM asked to name an agent routinely names
//     none, one that does not exist, or several. Parse → validate against the
//     roster → re-prompt with the error → after N attempts, degrade to a default
//     speaker and MARK the ledger degraded. Never match a free-text name loosely.
//  2. STALL COUNTER. When the ledger reports looping or no progress for
//     maxStalls consecutive turns, re-plan: ask the chair for a fresh approach,
//     record it, reset the counter. After a second exhausted run of stalls the
//     loop gives up rather than spinning.
//  3. HARD BACKSTOPS. maxTurns and the caller's context deadline always apply.
//     `satisfied` is advisory: an agent claiming completion cannot extend the
//     budget, and one that never claims it cannot run forever.

const (
	defaultMaxTurns  = 12
	defaultMaxStalls = 3

	// maxSelectorAttempts bounds the re-prompt ladder before degrading.
	maxSelectorAttempts = 3
)

// chairPrompt asks for the five-question ledger in a labeled format. A
// labeled reply parses far more reliably from a coding CLI than JSON does, and
// degrades gracefully when a field is missing.
func chairPrompt(st *State, roster []string, correction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the FACILITATOR of a planning meeting. You do not contribute opinions and you do not "+
		"keep the minutes; you direct the discussion and judge whether it is done.\n\nTopic: %s\n", st.Topic)
	if len(st.Agenda) > 0 {
		fmt.Fprintf(&b, "Agenda: %s\n", strings.Join(st.Agenda, " | "))
	}
	fmt.Fprintf(&b, "Participants (you may name ONLY these): %s\n\n", strings.Join(roster, ", "))
	b.WriteString("Read the transcript, then answer EXACTLY these six lines and nothing else:\n\n" +
		"SATISFIED: yes|no        (has the topic/agenda been fully addressed?)\n" +
		"LOOPING: yes|no          (are participants repeating points already made?)\n" +
		"PROGRESSING: yes|no      (did the last turn add something new?)\n" +
		"NEXT: <one participant name, exactly as spelled above>\n" +
		"INSTRUCTION: <one sentence telling that participant what to address>\n" +
		"REASON: <one sentence explaining this choice>\n\n" +
		"If SATISFIED is yes, still fill NEXT with any participant; it will be ignored.")
	if correction != "" {
		fmt.Fprintf(&b, "\n\nYOUR PREVIOUS REPLY WAS REJECTED: %s\nAnswer again, correctly.", correction)
	}
	return b.String()
}

// replanPrompt is the stall escape: the discussion is stuck, so re-derive an
// approach rather than calling on yet another participant.
func replanPrompt(st *State, roster []string) string {
	return fmt.Sprintf(
		"You are the FACILITATOR. The meeting on %q has STALLED — participants are repeating themselves "+
			"or adding nothing new for %d consecutive turns.\n\nParticipants: %s\n\n"+
			"In 2-4 sentences, state a NEW approach: what specific sub-question has gone unexamined, and who "+
			"should examine it. Do not restate the discussion so far.",
		st.Topic, st.maxStalls(), strings.Join(roster, ", "))
}

func parseYesNo(s string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Trim(s, "*_`.\"' "))) {
	case "yes", "y", "true", "1":
		return true
	}
	return false
}

// parseLedger reads the chair's labeled reply. Missing fields default
// conservatively: not satisfied, not looping, PROGRESSING true — so a garbled
// reply never ends the meeting and never triggers a spurious re-plan. The
// backstops, not the parse, are what bound the loop.
func parseLedger(s string) *Ledger {
	l := &Ledger{Progressing: true}
	for line := range strings.SplitSeq(s, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToUpper(strings.TrimSpace(strings.Trim(key, "*_`-# "))) {
		case "SATISFIED":
			l.Satisfied = parseYesNo(val)
		case "LOOPING":
			l.Looping = parseYesNo(val)
		case "PROGRESSING":
			l.Progressing = parseYesNo(val)
		case "NEXT":
			l.NextSpeaker = strings.TrimSpace(strings.Trim(val, "*_`\"'@ "))
		case "INSTRUCTION":
			l.Instruction = val
		case "REASON":
			l.Reason = val
		}
	}
	return l
}

// resolveSpeaker matches the chair's pick against the roster. Exact
// case-insensitive match only: loose substring matching is what produces the
// "coworker mentioned not found" bug class, where an agent named `code-reviewer`
// silently routes to `reviewer`.
func resolveSpeaker(name string, roster []string) (string, bool) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", false
	}
	for _, p := range roster {
		if strings.EqualFold(p, n) {
			return p, true
		}
	}
	return "", false
}

// nextLedger runs the selector validation ladder: ask, validate, re-prompt with
// the specific error, and finally degrade. It always returns a usable ledger.
func nextLedger(ctx context.Context, st *State, roster []string, fallback string, runner chat.Runner) *Ledger {
	correction := ""
	for attempt := 0; attempt < maxSelectorAttempts; attempt++ {
		// nil persist: a selector turn is the chair deciding who speaks next, not a
		// contribution to the discussion. It has never been in the transcript and
		// must not start being — replayed as context it would teach the next agent
		// to emit ledgers instead of arguments.
		ev, err := invokeAgent(ctx, st, st.chair(), string(RoleChair),
			chairPrompt(st, roster, correction), "", runner, nil)
		if err != nil || statusOf(ev) != statusOK {
			correction = "you did not reply"
			continue
		}
		l := parseLedger(ev.Text)
		if l.Satisfied {
			return l // the pick is irrelevant once the request is satisfied
		}
		who, ok := resolveSpeaker(l.NextSpeaker, roster)
		if ok {
			l.NextSpeaker = who
			return l
		}
		if l.NextSpeaker == "" {
			correction = "you named no participant on the NEXT line"
		} else {
			correction = fmt.Sprintf("%q is not a participant; NEXT must be exactly one of: %s",
				l.NextSpeaker, strings.Join(roster, ", "))
		}
	}
	// Degrade rather than hang or route to a nonexistent agent — and say so.
	return &Ledger{
		Progressing: true,
		NextSpeaker: fallback,
		Degraded:    true,
		Reason: fmt.Sprintf("facilitator %s failed to name a valid participant in %d attempts; defaulting to %s",
			st.chair(), maxSelectorAttempts, fallback),
	}
}

// Deliberation reports how a chaired run ended. `Satisfied` is the only outcome
// the chair chose; the others are the orchestrator's backstops firing, and a
// caller must be able to tell them apart.
type Deliberation struct {
	Turns     int    `json:"turns"`
	Stalls    int    `json:"stalls"`
	Replans   int    `json:"replans"`
	Degraded  int    `json:"degraded"`
	Satisfied bool   `json:"satisfied"`
	StoppedBy string `json:"stopped_by"` // satisfied | max_turns | stalled | deadline
}

// runChaired drives the meeting with an agent chair until the request is
// satisfied or a backstop fires. Termination is owned here, never by the model.
func runChaired(ctx context.Context, st *State, runner chat.Runner) (*Deliberation, error) {
	roster := st.Participants
	if len(roster) == 0 {
		return nil, fmt.Errorf("meet: a facilitated meeting needs participants")
	}
	// Held for the WHOLE deliberation, not per round: a chaired meeting's rounds
	// are a single control loop (the ledger from one round picks the speaker for
	// the next), so letting a second process in between rounds would interleave
	// exactly the state the chair is reasoning over.
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	res := &Deliberation{StoppedBy: "max_turns"}
	prev := roster[0]
	stalls, replans := 0, 0

	for res.Turns < st.maxTurns() {
		if err := ctx.Err(); err != nil {
			res.StoppedBy = "deadline"
			return res, nil
		}

		st.Round++
		_ = st.save()

		l := nextLedger(ctx, st, roster, prev, runner)
		if l.Degraded {
			res.Degraded++
		}
		_, _ = recordFull(st, Event{
			Round: st.Round, Speaker: st.chair(), Role: string(RoleChair), Kind: "ledger",
			Text: ledgerLine(l), Ledger: l,
		})

		if l.Satisfied {
			res.Satisfied, res.StoppedBy = true, "satisfied"
			return res, nil
		}

		// Stall detection precedes dispatch: calling on another participant to
		// repeat the loop is exactly the failure being defended against.
		if l.stalling() {
			stalls++
			res.Stalls++
			if stalls >= st.maxStalls() {
				replans++
				res.Replans++
				if replans > 1 {
					res.StoppedBy = "stalled"
					_, _ = record(st, "note", procedural(st), string(RoleChair),
						fmt.Sprintf("(meeting stopped: stalled through %d re-plans)", replans))
					return res, nil
				}
				// A replan IS recorded, so it persists through the callback — and
				// only when it is a usable replan, exactly as before. An unusable
				// one records nothing, which is a legitimate "nothing to persist"
				// rather than a failure, so the closure returns nil.
				_, _ = invokeAgent(ctx, st, st.chair(), string(RoleChair),
					replanPrompt(st, roster), "", runner,
					func(e Event) (Event, error) {
						if statusOf(e) != statusOK {
							return e, nil
						}
						e.Kind = "replan"
						e.Role = string(RoleChair)
						return e, appendEvent(st.ID, e)
					})
				stalls = 0
				continue // re-derive the ledger against the new plan
			}
		} else {
			stalls = 0
		}

		if _, ok := resolveSpeaker(l.NextSpeaker, roster); !ok {
			l.NextSpeaker = prev // defence in depth; nextLedger already guarantees this
		}
		q := l.Instruction
		if strings.TrimSpace(q) == "" {
			q = currentAgenda(st)
		}
		ev, _ := runTurn(ctx, st, l.NextSpeaker, q, runner)
		prev = ev.Speaker
		res.Turns++
	}
	return res, nil
}

// ledgerLine renders a ledger for the transcript and the minutes.
func ledgerLine(l *Ledger) string {
	flags := []string{}
	if l.Satisfied {
		flags = append(flags, "satisfied")
	}
	if l.Looping {
		flags = append(flags, "looping")
	}
	if !l.Progressing {
		flags = append(flags, "no-progress")
	}
	if l.Degraded {
		flags = append(flags, "degraded")
	}
	var b strings.Builder
	if l.Satisfied {
		b.WriteString("request satisfied")
	} else {
		fmt.Fprintf(&b, "next: %s", l.NextSpeaker)
	}
	if len(flags) > 0 {
		fmt.Fprintf(&b, " [%s]", strings.Join(flags, " "))
	}
	if l.Reason != "" {
		fmt.Fprintf(&b, " — %s", l.Reason)
	}
	return b.String()
}
