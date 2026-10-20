package dagrun

import (
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a session. It distinguishes a live session
// with no client attached (detached) from a session whose executor is gone
// (lost); both are terminal-free except exited and lost, which never change.
type State string

const (
	// StateRunning is a live session. Attach never leaves it.
	StateRunning State = "running"
	// StateDetached is a live session whose client disconnected. The process
	// keeps running; output keeps appending to the session log.
	StateDetached State = "detached"
	// StateExited is a session whose process exited. The exit code was
	// recorded exactly once.
	StateExited State = "exited"
	// StateLost is a session whose executor (process, node or host) is gone.
	// It is terminal and distinct from detached: nothing re-runs, nothing
	// migrates, and the session never becomes exited afterwards.
	StateLost State = "lost"
)

// Valid reports whether s is a known session state.
func (s State) Valid() bool {
	switch s {
	case StateRunning, StateDetached, StateExited, StateLost:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is exited or lost. No event leaves a terminal
// state.
func (s State) Terminal() bool {
	return s == StateExited || s == StateLost
}

// Kind is the kind of session: an interactive shell, a long-running agent, a
// one-shot batch workload, or a service.
type Kind string

const (
	// KindInteractive is one persistent shell under a PTY.
	KindInteractive Kind = "interactive"
	// KindAgent is a long-running agent run inside the allocation.
	KindAgent Kind = "long-running"
	// KindBatch is a one-shot workload.
	KindBatch Kind = "batch"
	// KindService is a deployed service.
	KindService Kind = "service"
)

// Valid reports whether k is a known session kind.
func (k Kind) Valid() bool {
	switch k {
	case KindInteractive, KindAgent, KindBatch, KindService:
		return true
	default:
		return false
	}
}

var (
	// ErrInvalidSession is returned by NewSession when the caller-supplied
	// identity or kind is missing or unknown.
	ErrInvalidSession = errors.New("dagrun: invalid session")
	// ErrEmptyClientID is returned when a transition names no client.
	ErrEmptyClientID = errors.New("dagrun: empty client id")
	// ErrNotHolder is returned when a client that does not hold the session
	// tries to detach it.
	ErrNotHolder = errors.New("dagrun: client does not hold the session")
	// ErrSecondExit is returned when an exit is recorded twice. The exit
	// code is recorded exactly once.
	ErrSecondExit = errors.New("dagrun: exit already recorded")
	// ErrIllegalTransition is returned for any event the current state does
	// not allow, including every event on a terminal (exited, lost) session.
	ErrIllegalTransition = errors.New("dagrun: illegal session transition")
)

// Session is the record of one session: identity, placement, holder and
// outcome. IDs and timestamps are caller-supplied; the package keeps no clock
// and no randomness. The zero value is not valid: build sessions with
// NewSession.
//
// The legal transitions are:
//
//	running  + attach -> running   (holder swaps; takeover)
//	running  + detach -> detached  (client disconnect, holder cleared)
//	running  + exit   -> exited    (exit code recorded)
//	running  + lost   -> lost      (executor loss, holder cleared)
//	detached + attach -> detached  (holder set, state unchanged)
//	detached + detach -> detached  (holder cleared)
//	detached + exit   -> exited
//	detached + lost   -> lost
//	exited|lost + anything -> error
//
// Attach never changes state, so a session that lost its client moves
// detached -> exited directly. No transition re-runs anything.
type Session struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspace_id"`
	RunID        string    `json:"run_id"`
	Kind         Kind      `json:"kind"`
	ExecutorHost string    `json:"executor_host"`
	ExecutorOS   string    `json:"executor_os"`
	ExecutorArch string    `json:"executor_arch"`
	State        State     `json:"state"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	Holder       string    `json:"holder"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NewSession builds the initial running session record. All identity and time
// comes from the caller. The holder may be empty for a session created
// without an attached client.
func NewSession(id, workspaceID, runID string, kind Kind, host, os, arch, holder string, now time.Time) (Session, error) {
	if id == "" {
		return Session{}, fmt.Errorf("%w: id is empty", ErrInvalidSession)
	}
	if workspaceID == "" {
		return Session{}, fmt.Errorf("%w: workspace id is empty", ErrInvalidSession)
	}
	if !kind.Valid() {
		return Session{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidSession, kind)
	}
	return Session{
		ID:           id,
		WorkspaceID:  workspaceID,
		RunID:        runID,
		Kind:         kind,
		ExecutorHost: host,
		ExecutorOS:   os,
		ExecutorArch: arch,
		State:        StateRunning,
		Holder:       holder,
		StartedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Attach records clientID as the session holder. It is allowed from running
// and detached and never changes state: attaching over a different live
// holder is a takeover. It fails on terminal sessions.
func (s Session) Attach(clientID string, now time.Time) (Session, error) {
	if clientID == "" {
		return Session{}, ErrEmptyClientID
	}
	if s.State.Terminal() {
		return Session{}, fmt.Errorf("%w: cannot attach client %q to %s session",
			ErrIllegalTransition, clientID, s.State)
	}
	next := copySession(s)
	next.Holder = clientID
	next.UpdatedAt = now
	return next, nil
}

// Detach records a client disconnect: the session stays alive but holderless.
// It is allowed from running and detached when clientID holds the session,
// and fails on terminal sessions.
func (s Session) Detach(clientID string, now time.Time) (Session, error) {
	if clientID == "" {
		return Session{}, ErrEmptyClientID
	}
	if s.State.Terminal() {
		return Session{}, fmt.Errorf("%w: cannot detach client %q from %s session",
			ErrIllegalTransition, clientID, s.State)
	}
	if s.Holder != clientID {
		return Session{}, fmt.Errorf("%w: holder is %q, not %q",
			ErrNotHolder, s.Holder, clientID)
	}
	next := copySession(s)
	next.State = StateDetached
	next.Holder = ""
	next.UpdatedAt = now
	return next, nil
}

// RecordExit records the process exit code exactly once. It is allowed from
// running and detached; a second exit is an error and a lost session never
// becomes exited.
func (s Session) RecordExit(code int, now time.Time) (Session, error) {
	if s.State == StateExited {
		return Session{}, fmt.Errorf("%w: exit code %d already recorded",
			ErrSecondExit, *s.ExitCode)
	}
	if s.State == StateLost {
		return Session{}, fmt.Errorf("%w: cannot record exit on lost session",
			ErrIllegalTransition)
	}
	next := copySession(s)
	next.State = StateExited
	next.ExitCode = &code
	next.UpdatedAt = now
	return next, nil
}

// MarkLost records executor loss. It is allowed from running and detached,
// clears the holder, and is terminal: a lost session is distinct from a
// detached one and never transitions again.
func (s Session) MarkLost(now time.Time) (Session, error) {
	if s.State.Terminal() {
		return Session{}, fmt.Errorf("%w: cannot mark %s session lost",
			ErrIllegalTransition, s.State)
	}
	next := copySession(s)
	next.State = StateLost
	next.Holder = ""
	next.UpdatedAt = now
	return next, nil
}

// copySession duplicates s including the exit-code pointer, so snapshots
// handed out to different readers never alias each other.
func copySession(s Session) Session {
	if s.ExitCode != nil {
		code := *s.ExitCode
		s.ExitCode = &code
	}
	return s
}
