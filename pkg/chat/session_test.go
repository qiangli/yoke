package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestPrepareSessionSocketRemovesStalePathBeforeReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stable-agent.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessionSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale control path survived preparation: %v", err)
	}
}

// Start must REFUSE a tool it cannot steer, rather than quietly handing back a
// one-shot.
//
// This is the whole contract. A one-shot runs its prompt and exits, so a steer
// sent to it arrives after the agent is gone — and every symptom of that looks
// like success: the socket accepts the bytes, the command prints "sent", the state
// goes to working, an answer comes back. `meet say` shipped in exactly that
// condition for months.
//
// A caller that asked for a conversation and got a monologue must be told.
func TestStartRefusesAToolWithNoInteractiveLaunch(t *testing.T) {
	_, err := Start(context.Background(), "definitely-not-a-registered-tool", SessionOptions{
		Prompt:  "hello",
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("Start accepted an unknown agent — a session with nothing on the other end " +
			"must fail loudly, not silently")
	}
}

func TestOpeningSendOnceCapsThePTYBoundaryAtOneWrite(t *testing.T) {
	if got := openingSendCap(true); got != 1 {
		t.Fatalf("exact-once opening send cap = %d, want 1", got)
	}
	if got := openingSendCap(false); got != maxOpenSends {
		t.Fatalf("ordinary chat opening send cap = %d, want %d", got, maxOpenSends)
	}
}

func TestClosePTYForceCancelsAgentThatIgnoresQuit(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	s := &Session{
		done: done,
		cancel: func() {
			close(cancelled)
			close(done)
		},
	}

	s.closePTY(10 * time.Millisecond)
	select {
	case <-cancelled:
	default:
		t.Fatal("Close left an agent alive after it ignored the graceful quit")
	}
}

func TestAbortStartCancelsPostLaunchSession(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	s := &Session{
		done: done,
		cancel: func() {
			close(cancelled)
			close(done)
		},
	}

	s.abortStart()
	select {
	case <-cancelled:
	default:
		t.Fatal("post-launch Start failure left its session running")
	}
}

// CanSteer is how a caller degrades LOUDLY. foreman consults it before falling
// back to replay; meet consults it before promising a chair it can interrupt.
func TestCanSteerNamesTheReason(t *testing.T) {
	ok, why := CanSteer("definitely-not-a-registered-tool")
	if ok {
		t.Fatal("CanSteer said yes to an agent that does not exist")
	}
	if strings.TrimSpace(why) == "" {
		t.Error("CanSteer refused without saying why — an operator who cannot steer " +
			"needs to know whether the tool lacks an interactive launch, the platform " +
			"lacks a pty, or the agent simply is not installed")
	}
}

// A turn boundary must exist even when the agent says NOTHING.
//
// WaitIdle keys off silence, so a naive implementation waits forever on the one
// failure it most needs to report: an agent that launched and never spoke. The
// idle clock is therefore seeded at launch, not at first output.
func TestWaitIdleReturnsOnATotallySilentAgent(t *testing.T) {
	s := &Session{done: make(chan struct{}), lastWrite: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.WaitIdle(ctx, 600*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle on a silent agent: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("WaitIdle hung on an agent that never spoke")
	}
}

// Turn returns only what was said SINCE the last turn — otherwise a foreman's
// history and a meeting's minutes would re-record the whole session on every
// message.
func TestTurnReturnsOnlyTheDelta(t *testing.T) {
	s := &Session{done: make(chan struct{}), lastWrite: time.Now()}
	w := &sessionWriter{s: s}

	_, _ = w.Write([]byte("first answer"))
	if got := s.Turn(); got != "first answer" {
		t.Fatalf("Turn 1 = %q", got)
	}
	_, _ = w.Write([]byte("second answer"))
	if got := s.Turn(); got != "second answer" {
		t.Errorf("Turn 2 = %q — it must not replay the first", got)
	}
	if got := s.Turn(); got != "" {
		t.Errorf("Turn 3 = %q — nothing was said, so nothing is the answer", got)
	}
	if got := s.Output(); got != "first answersecond answer" {
		t.Errorf("Output must still hold the WHOLE transcript: %q", got)
	}
}

// A session with no control channel cannot be steered, and must say so rather than
// returning nil.
func TestSayWithoutAControlChannelFails(t *testing.T) {
	s := &Session{Nick: "Ada", done: make(chan struct{})}
	if err := s.Say("stop"); err == nil {
		t.Fatal("Say succeeded with no control socket")
	}
	if err := s.Say("   "); err == nil {
		t.Fatal("Say accepted an empty steer")
	}
}

// --- attended sessions ----------------------------------------------------

// steerableKillSwitchTool registers a tool whose INTERACTIVE launch carries an
// auto-approve kill-switch — the shape every real coding CLI ships (claude's
// --dangerously-skip-permissions is the one in the wild).
func steerableKillSwitchTool(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "killswitcher", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{
			Exec:      "killswitcher --dangerously-skip-permissions -p {prompt}",
			SteerExec: "killswitcher --dangerously-skip-permissions",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { newCatalog = prev })
}

// THE REGRESSION. ycode's /agent attach is a human driving another agent through
// a proxying front end, and it was refused outright on an ordinary host: the
// launcher saw a kill-switch with nothing containing it, and SessionOptions had
// no way to say that a person was sitting right there. Every /agent attach to a
// kill-switch tool failed on exactly the machine where it is most obviously safe.
//
// Attended is that signal, and it must reach the launcher.
func TestAttendedSessionLaunchesOnAnUncontainedHost(t *testing.T) {
	steerableKillSwitchTool(t)
	stubContainerized(t, false)

	l, err := resolveLaunch("killswitcher", SessionOptions{Attended: true}.launchOptions())
	if err != nil {
		t.Fatalf("an attended session must launch on an uncontained host: %v", err)
	}
	for _, a := range l.Args {
		if a == "--dangerously-skip-permissions" {
			t.Fatalf("attended must STRIP the kill-switch, not carry it: %q", l.Args)
		}
	}
}

// The guard is not weakened for everyone else. A session nobody is watching —
// foreman, meet, coach — still cannot hand an agent its own kill-switch.
func TestUnattendedSessionIsStillRefused(t *testing.T) {
	steerableKillSwitchTool(t)
	stubContainerized(t, false)

	if _, err := resolveLaunch("killswitcher", SessionOptions{}.launchOptions()); err == nil {
		t.Fatal("an unattended session was permitted with its approval gate disabled")
	}
}

// ReadOnly is stricter than Attended and wins, so plan mode stays read-only even
// with a human driving. Same precedence Interact uses.
func TestReadOnlyBeatsAttended(t *testing.T) {
	if got := (SessionOptions{Attended: true, ReadOnly: true}).launchOptions(); got.Attended {
		t.Fatalf("ReadOnly must win over Attended: %+v", got)
	}
}

// Resolution and governance must be handed the IDENTICAL options — a session
// resolved as attended and governed as something else is the drift this helper
// exists to prevent.
func TestLaunchOptionsCarriesTheSessionsIntent(t *testing.T) {
	opt := SessionOptions{Cwd: "/w", Attended: true, AllowPremium: true}
	got := opt.launchOptions()
	if !got.Steer {
		t.Error("a session always resolves the interactive launch")
	}
	if got.Cwd != "/w" || !got.Attended || !got.AllowPremium {
		t.Errorf("launchOptions dropped the session's intent: %+v", got)
	}
}

// --- the idle clock -------------------------------------------------------

// THE REGRESSION. A turn boundary guessed from silence must not be satisfied by
// silence that happened BEFORE the agent was asked anything.
//
// An agent sitting idle since the session opened is already well past `quiet` at
// the instant a steer reaches it, so the first tick after Say returned "the turn
// is over" before the model had produced a character. The caller read an empty
// Turn() and told the operator the agent produced no output — true of the bytes,
// false of the agent, and the failure looked exactly like a broken agent rather
// than a broken clock. ycode's /agent attach hit it on EVERY message.
func TestWaitIdleDoesNotCountSilenceFromBeforeTheSteer(t *testing.T) {
	s := &Session{done: make(chan struct{}), lastWrite: time.Now().Add(-time.Minute)}

	// Say without a control channel fails, so stamp the steer the way say() does.
	s.mu.Lock()
	s.lastSteer = time.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	if err := s.WaitIdle(ctx, 30*time.Second); err == nil {
		t.Fatal("WaitIdle declared the turn over using silence that predated the steer")
	}
}

// Each write buys the agent another `quiet`, so a turn that streams with pauses
// shorter than the budget is not cut in half.
func TestWaitIdleEndsOneQuietPeriodAfterTheLastWrite(t *testing.T) {
	s := &Session{done: make(chan struct{})}
	s.mu.Lock()
	s.lastSteer = time.Now().Add(-time.Minute)
	s.lastWrite = time.Now().Add(-time.Minute)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.WaitIdle(ctx, 600*time.Millisecond); err != nil {
		t.Fatalf("a long-quiet agent must yield a boundary: %v", err)
	}
}

// An agent that answers NOTHING still has to end the wait — one quiet period
// after being asked, not never. Silence is a bad answer, but a hang is worse.
func TestWaitIdleTerminatesOnAnAgentThatNeverAnswers(t *testing.T) {
	s := &Session{done: make(chan struct{}), lastWrite: time.Now().Add(-time.Hour)}
	s.mu.Lock()
	s.lastSteer = time.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.WaitIdle(ctx, 700*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle hung on a silent agent: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("boundary took %s — it must arrive one quiet period after the steer", time.Since(start))
	}
}
