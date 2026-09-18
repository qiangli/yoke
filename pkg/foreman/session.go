package foreman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

type Options struct {
	ID              string
	Goal            string
	Agent           string
	Role            string
	Cwd             string
	Root            string
	MaxRuntime      time.Duration
	Runner          chat.Runner
	OpeningSendOnce bool
	// AllowUnsafe records the operator's explicit authorization for this
	// unattended managed session to keep the agent CLI's approval-bypass flag.
	// It is persisted because a detached start is reopened by the serve process
	// before the live chat session is launched.
	AllowUnsafe bool

	// Eager brings the agent up AT Start instead of on the first message.
	//
	// The default is lazy and that is right for a work session: an operator who
	// starts a foreman and never tells it anything should not be paying for a
	// model to sit at a prompt. But an OWNER session is started so that a name
	// becomes REACHABLE, and until the agent is up there is no control socket
	// and no room card advertising delivery — so mail addressed to the owner has
	// nowhere to land. That window, between `sprint start` and whatever message
	// happens to arrive first, is exactly the unreachable-owner state the sprint
	// admission gate exists to refuse.
	//
	// So: eager for a seat, lazy for a task.
	Eager bool
}

type Session struct {
	store  Store
	state  State
	runner chat.Runner
	kbNote *string    // cached host-kb preamble for the session goal
	mu     sync.Mutex // guards state; HELD FOR THE WHOLE TURN

	// hist is the bounded continuity projection over the history artifact. It
	// carries its own lock (see history.go): a mid-turn steer must be recorded
	// while a turn holds s.mu.
	hist continuity

	// live is guarded by its OWN lock, deliberately.
	//
	// Apply holds s.mu for the entire duration of a turn — that is correct, a turn
	// is one atomic thing. But a STEER must reach the agent *while that turn is
	// running*, which means it cannot wait on s.mu: it would block until the very
	// turn it was trying to interrupt had already finished. A steer that waits for
	// the turn to end is not a steer, it is a note left on the desk.
	liveMu  sync.Mutex
	live    *chat.Session
	logFile *os.File // the live agent's output, tee'd for `foreman log`

	// stopCh is independent of s.mu on purpose. A turn holds s.mu until the
	// agent returns, so routing stop through Apply would queue the stop behind
	// the very process it must terminate. The control server owns this channel
	// and cancels the turn context before recording the terminal state.
	stopCh   chan string
	stopOnce sync.Once
}

func Start(ctx context.Context, opt Options) (*Session, error) {
	id := strings.TrimSpace(opt.ID)
	if id == "" {
		id = newID()
	}
	goal := strings.TrimSpace(opt.Goal)
	if goal == "" {
		return nil, errors.New("foreman: goal required")
	}
	store := NewStore(opt.Root, id)
	now := time.Now().UTC()
	st := State{
		ID:              id,
		Goal:            goal,
		Status:          StatusIdle,
		CtlSock:         store.CtlSockPath(),
		Agent:           opt.Agent,
		Role:            opt.Role,
		Cwd:             opt.Cwd,
		CreatedAt:       now,
		UpdatedAt:       now,
		OpeningSendOnce: opt.OpeningSendOnce,
		AllowUnsafe:     opt.AllowUnsafe,
	}
	if opt.MaxRuntime > 0 {
		st.MaxRuntime = opt.MaxRuntime.String()
		st.Deadline = now.Add(opt.MaxRuntime)
	}
	st, err := store.Commit(st)
	if err != nil {
		return nil, err
	}
	s := &Session{store: store, state: st, runner: opt.Runner, stopCh: make(chan string, 1)}
	if err := s.hist.replay(store); err != nil {
		return nil, err
	}
	if opt.Eager {
		if err := s.attachEagerly(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// attachEagerly brings the agent up during Start, so the session is addressable
// before anyone has spoken to it.
//
// The opening message is the GOAL, which is what steer() would have sent as the
// first message anyway ("the opening message carries the goal and any host-kb
// preamble"). This is the existing path run at a different time, not a new one.
//
// TWO LOCKING FACTS MAKE THE SHAPE OF THIS FUNCTION. attach() writes state and
// calls persistLocked, so it must run under s.mu. closeLive() takes s.mu itself,
// so the cleanup CANNOT run under it — doing the obvious thing and deferring a
// close inside the locked region deadlocks. Hence: attach locked, release, then
// clean up.
//
// The cleanup is deliberately unconditional on the error path rather than
// conditional on how far attach got. attach only publishes s.live and s.logFile
// after chat.Start succeeds, so today there is nothing to reclaim on failure —
// but a caller that must know which half of a two-step failed is a caller that
// breaks when the two steps change. closeLive is a no-op on an empty session.
func (s *Session) attachEagerly(ctx context.Context) error {
	s.mu.Lock()
	err := s.attach(ctx, s.state.Goal)
	s.mu.Unlock()
	if err != nil {
		s.closeLive()
		return fmt.Errorf("foreman: eager attach: %w", err)
	}
	return nil
}

// Open resumes a session from its directory. Continuity is rebuilt from the
// history artifact, so the first prompt after a restart carries the same
// checkpoint — same decisions, same last result, same references — that the
// process which wrote it would have composed.
func Open(root, id string, runner chat.Runner) (*Session, error) {
	store := NewStore(root, id)
	st, err := store.LoadState()
	if err != nil {
		return nil, err
	}
	s := &Session{store: store, state: st, runner: runner, stopCh: make(chan string, 1)}
	if err := s.hist.replay(store); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) requestStop(reason string) {
	s.stopOnce.Do(func() { s.stopCh <- reason })
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) Store() Store {
	return s.store
}

func (s *Session) Enqueue(cmd Command) error {
	return s.store.AppendCommand(cmd)
}

func (s *Session) ProcessPending(ctx context.Context) error {
	cmds, err := s.store.LoadCommands()
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		return nil
	}
	if err := s.store.TruncateCommands(); err != nil {
		return err
	}
	for _, cmd := range cmds {
		if err := s.Apply(ctx, cmd); err != nil {
			return err
		}
	}
	return s.saveState()
}

// persistLocked writes state.json. The caller must ALREADY hold s.mu — it reads
// s.state directly rather than going through State(), which would re-take the
// lock and deadlock.
//
// It exists because status must be true DURING a turn, not merely after it.
// SaveState used to run only once Apply returned, so for the entire time an agent
// was working, `foreman status` reported "idle" and `steering: false` -- the file
// described a session that was doing nothing, while an agent burned tokens. An
// operator cannot supervise a run whose status only becomes true once there is
// nothing left to supervise.
func (s *Session) persistLocked() { _ = s.commitLocked() }

// commitLocked writes state.json through Store.Commit and adopts the committed
// record (seq/digest) so State() reports the sequence a supervisor will see.
// Identical content is a no-op by contract. The caller holds s.mu.
func (s *Session) commitLocked() error {
	st, err := s.store.Commit(s.state)
	if err != nil {
		return err
	}
	s.state = st
	return nil
}

// saveState snapshots and writes state while holding the same lock used by
// lifecycle transitions. State()+SaveState() is not equivalent: a stop can
// land between those calls and the stale snapshot can overwrite the terminal
// record even though each individual file replacement is atomic.
func (s *Session) saveState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked()
}

// setStatus moves the session out of (or between) non-blocked states; the
// blocker is cleared because it described a state the session has left.
func (s *Session) setStatus(status string) {
	s.state.Status = status
	if status != StatusBlocked {
		s.state.Blocker = ""
	}
}

// block records WHY the session is blocked alongside the status, so a
// supervisor reading a transition never has to open the log to learn it.
func (s *Session) block(reason string) {
	s.state.Status = StatusBlocked
	s.state.Blocker = strings.TrimSpace(reason)
}

// blockAndCommit makes a terminal turn failure observable before returning it.
// ProcessPending stops on Apply errors, so deferring persistence to its normal
// epilogue would leave state.json stuck at working indefinitely.
func (s *Session) blockAndCommit(reason string, cause error) error {
	s.block(reason)
	if err := s.commitLocked(); err != nil {
		return errors.Join(cause, fmt.Errorf("foreman: persist blocked state: %w", err))
	}
	return cause
}

// record appends one verbatim turn to the history artifact. A failed write is
// a blocker: the prompt's references would otherwise point at an entry that
// was never written.
func (s *Session) record(role, target, text string) error {
	_, err := s.hist.record(s.store, role, target, text)
	return err
}

func (s *Session) Apply(ctx context.Context, cmd Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// An accepted control command may have queued behind an active turn when
	// shutdown cancelled its context. Do not start it or overwrite terminal
	// state after that turn releases the lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(cmd.Verb)) {
	case CommandTell:
		if strings.TrimSpace(cmd.Message) == "" {
			return errors.New("foreman: tell message required")
		}
		if s.state.Paused {
			s.block("paused by operator")
			return s.record(RoleHuman, "", cmd.Message)
		}
		// Compose continuity BEFORE recording this message. The message belongs
		// once in the prompt below; including the just-recorded decision in the
		// checkpoint would repeat it as CurrentStep, Accepted decisions, Recent
		// turns, and Steering message.
		prior := s.continuationLocked()
		if err := s.record(RoleHuman, "", cmd.Message); err != nil {
			return s.blockAndCommit("history artifact: "+err.Error(), err)
		}
		s.setStatus(StatusWorking)
		s.state.CurrentStep = cmd.Message
		if err := s.commitLocked(); err != nil { // the turn has STARTED; say so now, not when it ends
			return err
		}
		if s.runner == nil && strings.TrimSpace(s.state.Agent) == "" && strings.TrimSpace(s.state.Role) == "" {
			s.setStatus(StatusIdle)
			return nil
		}

		// STEER a live agent when we can. `tell` means "lean over and say something
		// to the agent that is working right now" — and for most of this fleet's
		// tools that is now literally what it does.
		if ok, why := s.steerable(); ok {
			if err := s.steer(ctx, cmd.Message, prior); err != nil {
				// Falling through to the replay path would be the wrong kindness: it
				// would produce a plausible answer from a fresh agent and hide the fact
				// that the live one is gone.
				s.state.Steering = false
				s.state.SteerWhyNot = err.Error()
				return s.blockAndCommit("live agent: "+err.Error(), err)
			}
			s.setStatus(StatusIdle)
			break
		} else {
			// Not steerable. The replay path below still works — it is just a
			// different thing, and the state must not pretend otherwise.
			s.state.Steering = false
			s.state.SteerWhyNot = why
		}

		res, err := chat.Invoke(ctx, chat.Options{
			Agent:       s.state.Agent,
			Role:        s.state.Role,
			Instruction: s.composePrompt(cmd.Message, prior),
			Cwd:         s.state.Cwd,
			AllowUnsafe: s.state.AllowUnsafe,
		}, s.runner)
		if out := strings.TrimSpace(res.Output); out != "" {
			if rerr := s.record(RoleAgent, "", out); rerr != nil {
				return s.blockAndCommit("history artifact: "+rerr.Error(), rerr)
			}
		}
		if err != nil || res.ExitCode != 0 {
			if err != nil {
				return s.blockAndCommit("runner: "+err.Error(), err)
			}
			err = fmt.Errorf("foreman: runner exited %d", res.ExitCode)
			return s.blockAndCommit(err.Error(), err)
		}
		s.setStatus(StatusIdle)
	case CommandPause:
		s.state.Paused = true
		s.block("paused by operator")
	case CommandResume:
		s.state.Paused = false
		s.setStatus(StatusIdle)
	case CommandSkip:
		if strings.TrimSpace(cmd.Target) != "" {
			s.state.CurrentStep = "skip:" + strings.TrimSpace(cmd.Target)
		} else {
			s.state.CurrentStep = ""
		}
		s.setStatus(StatusIdle)
	case CommandPrio:
		if strings.TrimSpace(cmd.Target) != "" {
			s.state.DriveLease = strings.TrimSpace(cmd.Target) + ":" + strings.TrimSpace(cmd.Priority)
		} else {
			s.state.DriveLease = strings.TrimSpace(cmd.Priority)
		}
	case CommandKey:
		// Handled on the control path, which does not take s.mu — a key exists to
		// interrupt a turn, and Apply is holding the lock for that very turn.
		return nil
	case CommandStop:
		s.state.Stopped = true
		s.setStatus(StatusDone)
		s.state.StopReason = "stopped by operator"
	default:
		return fmt.Errorf("foreman: unknown command %q", cmd.Verb)
	}
	return nil
}

// composePrompt is the opening prompt of a live session and the whole prompt
// of a non-steerable (replay) turn.
//
// It carries the goal, the host-kb preamble, the bounded checkpoint + recent
// window (checkpoint.go) and the new message — and NOT the session history.
// The history is the artifact the checkpoint references. Its size is therefore
// goal + kb + message + at most ContinuationBudget, on turn 3 and on turn 300.
func (s *Session) composePrompt(msg string, prepared ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal:\n%s\n\n", s.state.Goal)
	if note := s.kbPreamble(); note != "" {
		b.WriteString(note)
		b.WriteByte('\n')
	}
	cont := ""
	if len(prepared) > 0 {
		cont = prepared[0]
	} else {
		cont = s.continuationLocked()
	}
	if cont != "" {
		b.WriteString(cont)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Steering message:\n%s", msg)
	return b.String()
}

// continuationLocked renders the checkpoint for the state the caller (holding
// s.mu) is looking at. It reads the continuity projection under its own lock.
func (s *Session) continuationLocked() string {
	return buildCheckpoint(s.state, s.store, s.hist.snapshot()).render()
}

func newID() string {
	return time.Now().UTC().Format("20060102-150405.000000000")
}
