package weave

import (
	"context"
	"io"
	"os"
	"os/exec"

	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/chat"
)

// The PTY runner used to live here. It moved to pkg/agentpty because `chat`
// needs it too — a meeting participant that hangs on a trust prompt is exactly
// as stuck as a weave worker — and weave already imports chat, so chat could not
// import weave without a cycle.
//
// What is left is the weave-shaped half: weave's guards, and weave's opinion
// about what a worker log should look like.

// runWeaveToolPTY launches a subagent under a PTY with weave's watchdogs and
// control socket. Kept as weave's name for it so the call sites — and the tests
// that gate this move — read exactly as they did before.
func runWeaveToolPTY(cmd *exec.Cmd, logSink io.Writer, guards weaveGuards) (int, string, chat.CoachReport, string, error) {
	// The reflex coach (P2a): attach the LLM-free loop detector to every run by
	// default. It tees the run's DECODED prose (below) into a pty-novelty
	// detector and, when a run churns without progress, ESC+Says it off the loop
	// through the same control socket `weave attach` uses. Off with BASHY_NO_COACH;
	// a no-op when there is no control socket to steer through.
	sink := logSink
	if sink != nil {
		sink = &weaveLockedWriter{w: sink}
	}
	var coach *chat.Coach
	if guards.ctlSock != "" && chat.ReflexEnabled() {
		pol := chat.DefaultCoachPolicy()
		// Tie the wall-clock budget to this run's watchdog: nudge at 25/50/75% so
		// the coach steers (and escalates) well BEFORE the hard max-runtime kill.
		if guards.maxRuntime > 0 {
			pol.SoftTimeBudget = guards.maxRuntime / 4
		}
		coach = chat.NewLineCoach(pol, chat.NewCtlSteerer(guards.ctlSock))
		// P2b: after the generic steer fails, escalate to an agent one band above
		// this run's agent for a content-full steer.
		coach.SetEscalation(context.Background(), guards.coachee, chat.BandGraduatedEscalator)
		// Use the normalized sink: logSink may legitimately be nil when weave is
		// relying on a tool event file for progress. Passing a nil writer to
		// io.MultiWriter constructs successfully but panics on the first event.
		sink = weaveCoachSink(sink, coach)
	}

	var activity <-chan struct{}
	var stopEvents func()
	if guards.eventsPath != "" {
		activity, stopEvents = followWeaveEventFile(guards.eventsPath, sink)
		defer stopEvents()
	}

	code, reason, err := agentpty.Run(cmd, sink, agentpty.Options{
		IdleTimeout:   guards.idleTimeout,
		Activity:      activity,
		MaxRuntime:    guards.maxRuntime,
		MemLimitBytes: guards.memLimitBytes,
		CtlSock:       guards.ctlSock,

		// weave's worker log is read by humans and by `weave wait --broker`, so
		// an agent's stream-json is decoded into prose rather than dumped raw.
		// A meeting wants the opposite — the raw lines, exactly as written — and
		// that disagreement is why the filter is injected rather than baked in.
		Filter: func(w io.Writer) (io.Writer, func() error) {
			sj := newWeaveStreamJSONLogWriter(w)
			return sj, sj.Flush
		},
	})

	var coachRep chat.CoachReport
	var coachMode string
	if coach != nil {
		coachRep = coach.Report()
		coachMode = coach.Mode()
	}
	chat.NoteCoach(coach, logSink)
	return code, reason, coachRep, coachMode, err
}

func weaveCoachSink(sink io.Writer, coach io.Writer) io.Writer {
	if sink == nil {
		return coach
	}
	return io.MultiWriter(sink, coach)
}

// weaveStdinIsTTY reports whether the calling process's stdin is a real
// terminal. Used to gate the auto-setsid + auto-log-file paths.
func weaveStdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
