package foreman

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/chat"
)

// What `foreman tell` used to do
//
// It composed the goal + the whole session history + the new message into one
// prompt, spawned a BRAND NEW agent process, waited for it to exit, and appended
// its output to the history. Every message. That has three problems, and the third
// is the one that matters:
//
//  1. It re-pays the entire conversation in tokens on every turn.
//  2. The agent has no continuity — no open files, no train of thought, nothing it
//     learned last turn except what got scraped from its stdout.
//  3. IT CANNOT INTERRUPT ANYTHING. The whole point of a foreman is to lean over
//     and say "not that file — the other one" WHILE the agent is working. Queuing
//     a message for the next spawn is not steering; it is leaving a note.
//
// And it was indistinguishable, from the outside, from a foreman that did steer.
// The operator typed `tell`, the state went to `working`, an answer came back. It
// looked right. It was a conversation with an agent that had already left.
//
// So: hold the agent OPEN. `tell` becomes a keystroke into a live session, which
// is what the word always meant.
//
// # When we cannot
//
// A tool with no interactive launch cannot be steered — there is nothing running
// to steer. For those the replay path is not a bug, it is the only thing that
// works, and the state says so out loud (`steering: false` + why). A silent
// downgrade here would recreate the exact failure this change exists to fix.

// quietPeriod is how long an agent must be silent before its turn is considered
// over. See chat.Session.WaitIdle: silence is the only turn boundary a terminal
// offers, and this is sized for a slow tool call, not for a human's patience.
const quietPeriod = 25 * time.Second

// steerable reports whether this session can hold its agent open.
//
// An injected Runner always wins: it is the test seam and the programmatic entry
// point, and a caller that supplied its own runner asked for exactly that runner.
func (s *Session) steerable() (bool, string) {
	if s.runner != nil {
		return false, "a runner was supplied by the caller"
	}
	agent := strings.TrimSpace(s.state.Agent)
	if agent == "" {
		// A role resolves to an agent, but the agent it resolves to may change
		// between turns — which is a fine property for one-shot dispatch and a
		// nonsensical one for a held-open session. Steer a named agent.
		return false, "no agent named (a role picks a fresh agent per turn; a session holds one open)"
	}
	return chat.CanSteer(agent)
}

// setLive / getLive guard s.live with liveMu, never s.mu — see the Session struct.
func (s *Session) setLive(l *chat.Session) {
	s.liveMu.Lock()
	s.live = l
	s.liveMu.Unlock()
}

func (s *Session) getLive() *chat.Session {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live
}

// TrySteer delivers a message to the agent RIGHT NOW, without waiting for the
// current turn to finish — which is the entire point of steering.
//
// It takes only liveMu, never s.mu. Apply holds s.mu for the whole turn, so a
// steer that went through Apply would block until the turn it meant to interrupt
// was already over, and then arrive as a fresh instruction to a finished agent.
// That is precisely the failure `foreman tell` had before: a correction that is
// always too late, and looks exactly like one that was not.
//
// Reports whether a live agent was there to hear it.
func (s *Session) TrySteer(msg string) (bool, error) {
	live := s.getLive()
	if live == nil || !live.Live() {
		return false, nil
	}
	if err := live.Say(msg); err != nil {
		return false, err
	}
	return true, nil
}

// noteSteer records a mid-turn steer in the history, WITHOUT taking s.mu — the
// turn it interrupted is holding that lock. The continuity projection is
// guarded by its own small lock so an operator's correction is never lost just
// because it landed while the agent was busy, which is the only time a
// correction ever lands. It goes to the artifact and into the checkpoint's
// accepted decisions like any other operator instruction.
func (s *Session) noteSteer(msg string) error {
	return s.record(RoleMidTurn, "", msg)
}

// Keystroke names TrySendKey accepts. These are the keys that mean something to
// every agent TUI in the fleet, and nothing else is offered — this is a control
// channel, not a remote keyboard.
const (
	KeyEsc   = "esc"    // interrupt: abandon the current turn, keep the session
	KeyEnter = "enter"  // submit whatever is sitting in the input box
	KeyCtrlC = "ctrl-c" // harder stop; most TUIs need it twice to quit
)

var keyBytes = map[string][]byte{
	KeyEsc:   {0x1b},
	KeyEnter: {'\r'},
	KeyCtrlC: {0x03},
}

// TrySendKey presses a KEY at the running agent, rather than saying something to
// it.
//
// A steer is a word in the agent's ear. It is delivered mid-turn, but every agent
// TUI in this fleet QUEUES it and reads it only when the current turn ends — which
// is fine for a course correction and useless for the one case where you most need
// control: an agent stuck in a tool loop, whose turn is never going to end.
//
// Observed live: an agy conductor made 224 tool calls of which only 22 were
// distinct, re-reading the same file 26 times. A queued message would have sat
// there forever, because the agent was never going to pause long enough to read
// it. Escape is the only thing that reaches an agent in that state.
//
// This is what agentpty.VerbatimFrame is FOR — a keystroke, not a sentence. The
// wire has always had two frame kinds; foreman only ever used one.
func (s *Session) TrySendKey(name string) (bool, error) {
	b, ok := keyBytes[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return false, fmt.Errorf("foreman: unknown key %q (want: esc, enter, ctrl-c)", name)
	}
	live := s.getLive()
	if live == nil || !live.Live() {
		return false, nil
	}
	return true, agentpty.SendFrame(live.CtlSock, agentpty.VerbatimFrame(b))
}

// attach opens the live session, using the first message as its opening prompt.
func (s *Session) attach(ctx context.Context, msg string, prepared ...string) error {
	// Tee the agent's output to a log the operator can tail. A tee, never a
	// redirect: what gets RECORDED in the history is byte-for-byte what it would
	// have been with nobody watching. Observing must not change the record.
	var sink io.Writer
	var openedLog *os.File
	if err := s.store.Ensure(); err == nil {
		if f, err := os.OpenFile(s.store.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			openedLog = f
			sink = f
		}
	}

	timeout, err := s.remainingRuntime()
	if err != nil {
		return err
	}
	live, err := chat.Start(ctx, s.state.Agent, s.liveSessionOptions(
		s.composePrompt(msg, prepared...), sink, timeout))
	if err != nil {
		if openedLog != nil {
			_ = openedLog.Close()
		}
		return err
	}
	s.liveMu.Lock()
	s.live = live
	s.logFile = openedLog
	s.liveMu.Unlock()
	s.state.Steering = true
	s.state.SteerWhyNot = ""
	s.state.Binding = live.Agent
	// The agent is UP and reachable. Persist immediately: an operator watching
	// `foreman status` must be able to see that this session is steerable while the
	// turn is still running, which is the only time steering it would do any good.
	s.persistLocked()
	return nil
}

func (s *Session) liveSessionOptions(prompt string, sink io.Writer, timeout time.Duration) chat.SessionOptions {
	return chat.SessionOptions{
		Prompt:          prompt,
		OpeningSendOnce: s.state.OpeningSendOnce,
		Cwd:             s.state.Cwd,
		Stream:          sink,
		Timeout:         timeout,
		ReadOnly:        false, // a foreman's agent is here to DO the work
		AllowUnsafe:     s.state.AllowUnsafe,
		Mode:            "foreman",
		Task:            s.state.ID,
	}
}

func (s *Session) remainingRuntime() (time.Duration, error) {
	if s.state.Deadline.IsZero() {
		return 0, nil
	}
	remaining := time.Until(s.state.Deadline)
	if remaining <= 0 {
		return 0, fmt.Errorf("foreman: max runtime %s already expired", s.state.MaxRuntime)
	}
	return remaining, nil
}

// steer sends one message to the live agent and records what it said back.
//
// The opening message carries the goal and any host-kb preamble. Every message
// after it is sent RAW — the agent already has the conversation; replaying it
// would be both wasteful and confusing, since the agent would see its own words
// quoted back at it as if they were new.
func (s *Session) steer(ctx context.Context, msg string, prepared ...string) error {
	live := s.getLive()
	if live == nil || !live.Live() {
		if err := s.attach(ctx, msg, prepared...); err != nil {
			return err
		}
		live = s.getLive()
	} else if err := live.Say(msg); err != nil {
		return err
	}
	// Silence means the agent has stopped talking. It does NOT mean the agent has
	// finished thinking, and it certainly does not mean the agent did what it was
	// asked — for THAT, read the artifacts, not the terminal.
	_ = live.WaitIdle(ctx, quietPeriod)

	if out := strings.TrimSpace(chat.SanitizeTurn(live.Turn())); out != "" {
		if err := s.record(RoleAgent, "", out); err != nil {
			return err
		}
	}
	if !live.Live() {
		// It left. Say so rather than letting the next tell silently start a fresh
		// agent that knows nothing about this conversation.
		if _, err := live.Wait(); err != nil {
			s.setLive(nil)
			s.state.Steering = false
			s.state.SteerWhyNot = "the agent exited: " + err.Error()
			return nil
		}
		s.setLive(nil)
		s.state.Steering = false
		s.state.SteerWhyNot = "the agent exited"
	}
	return nil
}

// Close ends the live agent, if there is one. A foreman that is done with a
// session must not leave an agent sitting at a prompt forever.
func (s *Session) Close() { s.closeLive() }

// closeLive ends the live agent.
func (s *Session) closeLive() {
	s.liveMu.Lock()
	live := s.live
	logFile := s.logFile
	s.live = nil
	s.logFile = nil
	s.liveMu.Unlock()
	if live != nil {
		live.Close()
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	s.mu.Lock()
	s.state.Steering = false
	s.persistLocked()
	s.mu.Unlock()
}
