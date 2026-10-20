package dagrun

import (
	"errors"
	"testing"
	"time"
)

// sessClock returns a fixed caller-supplied timestamp so tests never depend
// on a hidden clock inside the package.
func sessClock() time.Time {
	return time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
}

const (
	sessFixtureID  = "01K81SESS000000000000000001"
	sessFixtureWS  = "01K81WORK000000000000000001"
	sessFixtureRun = "01K81RUN0000000000000000001"
)

// newSessFixture builds the standard running session used across tests:
// holder client-a on a Linux executor.
func newSessFixture(t *testing.T) Session {
	t.Helper()
	s, err := NewSession(
		sessFixtureID, sessFixtureWS, sessFixtureRun,
		KindInteractive, "exec-1", "linux", "amd64",
		"client-a", sessClock(),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

func TestSessionNewRequiresIDs(t *testing.T) {
	now := sessClock()
	if _, err := NewSession("", sessFixtureWS, sessFixtureRun, KindInteractive, "exec-1", "linux", "amd64", "client-a", now); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("empty id: got %v, want ErrInvalidSession", err)
	}
	if _, err := NewSession(sessFixtureID, "", sessFixtureRun, KindInteractive, "exec-1", "linux", "amd64", "client-a", now); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("empty workspace: got %v, want ErrInvalidSession", err)
	}
	if _, err := NewSession(sessFixtureID, sessFixtureWS, sessFixtureRun, "bogus-kind", "exec-1", "linux", "amd64", "client-a", now); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("unknown kind: got %v, want ErrInvalidSession", err)
	}

	s := newSessFixture(t)
	if s.State != StateRunning {
		t.Fatalf("initial state = %q, want %q", s.State, StateRunning)
	}
	if s.ExitCode != nil {
		t.Fatalf("initial ExitCode = %v, want nil", *s.ExitCode)
	}
	if !s.StartedAt.Equal(now) || !s.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps not caller-supplied: %+v", s)
	}
}

func TestSessionTransitionTable(t *testing.T) {
	now := sessClock()
	later := now.Add(time.Minute)

	// Every (state, event) pair: whether it is legal.
	cases := []struct {
		name  string
		from  State
		event string
		want  State
		ok    bool
	}{
		{"running attach stays running", StateRunning, "attach", StateRunning, true},
		{"running detach", StateRunning, "detach", StateDetached, true},
		{"running exit", StateRunning, "exit", StateExited, true},
		{"running lost", StateRunning, "lost", StateLost, true},
		{"detached attach stays detached", StateDetached, "attach", StateDetached, true},
		{"detached detach stays detached", StateDetached, "detach", StateDetached, true},
		{"detached exit", StateDetached, "exit", StateExited, true},
		{"detached lost", StateDetached, "lost", StateLost, true},
		{"exited attach rejected", StateExited, "attach", StateExited, false},
		{"exited detach rejected", StateExited, "detach", StateExited, false},
		{"exited exit rejected", StateExited, "exit", StateExited, false},
		{"exited lost rejected", StateExited, "lost", StateExited, false},
		{"lost attach rejected", StateLost, "attach", StateLost, false},
		{"lost detach rejected", StateLost, "detach", StateLost, false},
		{"lost exit rejected", StateLost, "exit", StateLost, false},
		{"lost lost rejected", StateLost, "lost", StateLost, false},
	}

	base := newSessFixture(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.State = tc.from
			s.Holder = "client-a"
			if tc.from == StateExited {
				code := 7
				s.ExitCode = &code
			}
			var (
				next Session
				err  error
			)
			switch tc.event {
			case "attach":
				next, err = s.Attach("client-b", later)
			case "detach":
				next, err = s.Detach("client-a", later)
			case "exit":
				next, err = s.RecordExit(3, later)
			case "lost":
				next, err = s.MarkLost(later)
			}
			if tc.ok {
				if err != nil {
					t.Fatalf("%s from %s: unexpected error %v", tc.event, tc.from, err)
				}
				if next.State != tc.want {
					t.Fatalf("%s from %s: state = %q, want %q", tc.event, tc.from, next.State, tc.want)
				}
				if !next.UpdatedAt.Equal(later) {
					t.Fatalf("%s: UpdatedAt not caller-supplied: %v", tc.event, next.UpdatedAt)
				}
				if !next.StartedAt.Equal(s.StartedAt) {
					t.Fatalf("%s: StartedAt changed by transition", tc.event)
				}
			} else if err == nil {
				t.Fatalf("%s from %s: expected error, got state %q", tc.event, tc.from, next.State)
			}
		})
	}
}

func TestSessionExitRecordedOnce(t *testing.T) {
	s := newSessFixture(t)
	now := sessClock()

	exited, err := s.RecordExit(7, now)
	if err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	if exited.State != StateExited {
		t.Fatalf("state = %q, want exited", exited.State)
	}
	if exited.ExitCode == nil || *exited.ExitCode != 7 {
		t.Fatalf("ExitCode = %v, want 7", exited.ExitCode)
	}

	if _, err := exited.RecordExit(7, now.Add(time.Second)); !errors.Is(err, ErrSecondExit) {
		t.Fatalf("second RecordExit: got %v, want ErrSecondExit", err)
	}
	if _, err := exited.RecordExit(0, now.Add(time.Second)); !errors.Is(err, ErrSecondExit) {
		t.Fatalf("second RecordExit with new code: got %v, want ErrSecondExit", err)
	}

	// Exit from detached also works exactly once.
	detached, err := s.Detach("client-a", now)
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	exited2, err := detached.RecordExit(7, now)
	if err != nil {
		t.Fatalf("RecordExit from detached: %v", err)
	}
	if exited2.State != StateExited || *exited2.ExitCode != 7 {
		t.Fatalf("detached exit = %+v, want exited with code 7", exited2)
	}
}

func TestSessionLostIsNotDetached(t *testing.T) {
	s := newSessFixture(t)
	now := sessClock()

	lost, err := s.MarkLost(now)
	if err != nil {
		t.Fatalf("MarkLost: %v", err)
	}
	if lost.State == StateDetached {
		t.Fatal("lost session reports detached: the two states must be distinct")
	}
	if lost.State != StateLost {
		t.Fatalf("state = %q, want lost", lost.State)
	}
	if lost.ExitCode != nil {
		t.Fatalf("lost session carries ExitCode %v, want nil (no exit was recorded)", *lost.ExitCode)
	}
	if lost.Holder != "" {
		t.Fatalf("lost session holder = %q, want empty (no client can hold a dead executor)", lost.Holder)
	}

	// Terminal: nothing leaves lost, and a lost session never becomes exited.
	if _, err := lost.Attach("client-b", now); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("attach to lost: got %v, want ErrIllegalTransition", err)
	}
	if _, err := lost.RecordExit(0, now); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("exit on lost: got %v, want ErrIllegalTransition (lost is not an exit)", err)
	}
	if _, err := lost.Detach("client-a", now); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("detach on lost: got %v, want ErrIllegalTransition", err)
	}
	if _, err := lost.MarkLost(now); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second MarkLost: got %v, want ErrIllegalTransition", err)
	}
}

func TestSessionDetachRequiresHolder(t *testing.T) {
	s := newSessFixture(t)
	now := sessClock()

	if _, err := s.Detach("client-b", now); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("detach by non-holder: got %v, want ErrNotHolder", err)
	}
	if _, err := s.Detach("", now); !errors.Is(err, ErrEmptyClientID) {
		t.Fatalf("detach with empty client: got %v, want ErrEmptyClientID", err)
	}
	if _, err := s.Attach("", now); !errors.Is(err, ErrEmptyClientID) {
		t.Fatalf("attach with empty client: got %v, want ErrEmptyClientID", err)
	}

	// Takeover at the record level: a different client may attach over a
	// running holder; the record just swaps the holder (eviction of the live
	// stream is the Hub's job).
	taken, err := s.Attach("client-b", now)
	if err != nil {
		t.Fatalf("takeover Attach: %v", err)
	}
	if taken.Holder != "client-b" {
		t.Fatalf("holder = %q, want client-b", taken.Holder)
	}
	if taken.State != StateRunning {
		t.Fatalf("takeover changed state to %q, want running (attach never changes state)", taken.State)
	}
}

func TestSessionTransitionsNeverRerun(t *testing.T) {
	s := newSessFixture(t)
	now := sessClock()

	// A transition returns a new value; the input is unchanged.
	detached, err := s.Detach("client-a", now)
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if s.State != StateRunning || s.Holder != "client-a" {
		t.Fatalf("Detach mutated its input: %+v", s)
	}
	if detached.State != StateDetached || detached.Holder != "" {
		t.Fatalf("Detach result = %+v, want detached with empty holder", detached)
	}
}
