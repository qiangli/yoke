package meet

import (
	"context"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

// A steerable turn holds the speaker OPEN.
//
// `meet say` has always written to a control socket. What it never had was
// anything on the other end: a meeting's turns were headless one-shots — the tool
// runs the prompt and exits — so the socket existed, the write succeeded, the
// command printed "→ Sable (round 2): stay on the gate question", and the agent
// never heard a word of it. It reported success and delivered nothing, which is
// the worst thing a control channel can do.
//
// In a steerable meeting the speaker is launched through its INTERACTIVE contract
// (`steer_exec:`) and left running for the whole turn, so a line from the chair
// arrives as keystrokes while the agent is still composing its answer.
//
// # What it costs, stated plainly
//
// A headless turn ends when the process exits — an exact boundary, for free. A
// live turn has no boundary: the agent just stops typing. So it ends on silence
// (chat.Session.WaitIdle), which means every turn pays a quiet period on the way
// out, plus a TUI's startup on the way in. That is why `--steerable` is a flag a
// chair asks for and not the default.
//
// It is also why the FIRST-PARTY harness matters: with a real event stream, a
// turn's end is a fact the agent reports, not a silence we interpret.

// turnQuiet is how long a speaker must say nothing before its turn is over.
//
// Sized for the slowest thing an agent might legitimately pause for mid-answer (a
// tool call, a re-prompt), not for how long a person is willing to sit there. Too
// short truncates a thinking agent mid-thought and records the fragment as its
// considered position — which would put words in a participant's mouth, in the
// minutes, permanently.
const turnQuiet = 25 * time.Second

// sessionTurn runs one participant's turn as a live, steerable session.
func sessionTurn(ctx context.Context, st *State, name, role, instruction, question string,
	budget time.Duration, start time.Time, persist func(Event) (Event, error)) (Event, error) {

	events, _ := readTranscript(st.ID)

	// The same prompt the headless path builds. A participant must be asked exactly
	// the same question whichever way it was launched, or the meeting's mode would
	// silently change what it is a meeting ABOUT.
	prompt, err := chat.BuildPrompt(chat.Options{
		Instruction: instruction,
		Context:     []string{transcriptContext(events)},
		Files:       st.Context,
		Cwd:         st.Cwd,
	})
	if err != nil {
		// No floor was ever claimed, so there is nothing to pair — but the turn is
		// still a turn and still belongs in the record.
		ev := classifyTurn(st, name, question, "", 2, err, time.Since(start), budget)
		_ = persistThenStatus(ev, persist)
		return ev, err
	}

	live := newLiveWriter(st, name, role, "")
	// Backstop pairing, as on the headless path: no exit from here may leave a
	// `speaking` without a `spoke`. Once-only, so the explicit closes below win.
	defer live.close(statusError)

	steerReadOnly, steerAllowUnsafe := turnAuthority()
	sess, err := chat.Start(ctx, name, chat.SessionOptions{
		Prompt:  prompt,
		Cwd:     st.Cwd,
		Timeout: budget,
		Stream:  live,
		// The steered seat gets the same authority as the one-shot seat — a seat
		// that may act when it answers once and may not when it can be talked to
		// mid-turn would be an accident of which code path started it. See
		// turnAuthority for what this grants and how to restore read-only.
		ReadOnly:    steerReadOnly,
		AllowUnsafe: steerAllowUnsafe,
		Mode:        "meet",
	})
	if err != nil {
		ev := classifyTurn(st, name, question, "", 2, err, time.Since(start), budget)
		live.close(persistThenStatus(ev, persist))
		return ev, err
	}
	defer sess.Close()

	// Publish the control socket so `meet say` can find the floor. It is set AFTER
	// the session is up, so a chair can never be told to steer an agent that is not
	// there yet.
	live.setCtlSock(sess.CtlSock)

	if err := sess.WaitIdle(ctx, turnQuiet); err != nil {
		ev := classifyTurn(st, name, question, sess.Output(), 124, err, time.Since(start), budget)
		live.close(persistThenStatus(ev, persist))
		return ev, err
	}

	ev := classifyTurn(st, name, question, sess.Turn(), 0, nil, time.Since(start), budget)
	live.close(persistThenStatus(ev, persist))
	return ev, nil
}
