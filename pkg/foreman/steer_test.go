package foreman

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

func TestLiveSessionOptionsCarryExplicitUnsafeAuthorization(t *testing.T) {
	s := &Session{state: State{
		ID: "sprint-115-manager", Agent: "claude-opus5", Cwd: "/work",
		OpeningSendOnce: true, AllowUnsafe: true,
	}}
	sink := io.Discard
	got := s.liveSessionOptions("start sprint", sink, 3*time.Minute)
	if !got.AllowUnsafe || !got.OpeningSendOnce || got.Prompt != "start sprint" ||
		got.Cwd != "/work" || got.Stream != sink || got.Timeout != 3*time.Minute ||
		got.Mode != "foreman" || got.Task != "sprint-115-manager" {
		t.Fatalf("live session options lost detached policy: %+v", got)
	}
}

// An injected Runner is the test seam AND the programmatic entry point. A caller
// that supplied its own runner asked for exactly that runner — it must not be
// quietly replaced by a live pty session.
func TestAnInjectedRunnerIsNeverSteered(t *testing.T) {
	s := &Session{state: State{Agent: "claude"}, runner: &recordingRunner{}}
	ok, why := s.steerable()
	if ok {
		t.Fatal("a session with an injected runner must not open a live pty session")
	}
	if !strings.Contains(why, "runner") {
		t.Errorf("the reason must name the runner, got %q", why)
	}
}

// A ROLE picks a fresh agent per turn. That is fine for one-shot dispatch and
// nonsense for a session you hold open: you would be holding open whichever agent
// the role happened to resolve to first, while the role went on meaning something
// else.
func TestARoleIsNotSteerable(t *testing.T) {
	s := &Session{state: State{Role: "coder"}}
	ok, why := s.steerable()
	if ok {
		t.Fatal("a role must not be steered — it names no particular agent")
	}
	if !strings.Contains(why, "agent") {
		t.Errorf("the reason must explain that a session holds ONE agent open, got %q", why)
	}
}

// THE HONESTY CONTRACT.
//
// `tell` on a non-steerable agent still works — it replays the conversation into a
// fresh one-shot — and that is a genuinely different act from interrupting a
// running agent. From the outside the two are indistinguishable: the operator
// types tell, the status goes to working, an answer comes back.
//
// So the state must say which one happened. An operator who believes they
// interrupted an agent, and did not, has been lied to by silence.
func TestReplayedTellIsRecordedAsNotSteering(t *testing.T) {
	r := &recordingRunner{}
	s := &Session{
		store:  NewStore(t.TempDir(), "t1"),
		state:  State{ID: "t1", Goal: "ship the thing", Agent: "stub", Status: StatusIdle},
		runner: r,
	}
	if err := s.Apply(context.Background(), Command{Verb: CommandTell, Message: "use the other file"}); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Steering {
		t.Error("state claims it steered a live agent, but the message was replayed into a fresh one-shot")
	}
	if strings.TrimSpace(st.SteerWhyNot) == "" {
		t.Error("a foreman that could not steer must say why — otherwise `steering: false` " +
			"is indistinguishable from a bug")
	}
	if len(r.prompts) != 1 {
		t.Fatalf("expected one replayed invocation, got %d", len(r.prompts))
	}
	// The replay path carries the whole conversation, because the fresh agent has
	// no memory of it. That cost is exactly what the live session eliminates.
	if !strings.Contains(r.prompts[0], "ship the thing") {
		t.Error("the replayed prompt must carry the goal — a fresh agent knows nothing")
	}
}

// chat.CanSteer is the single predicate both foreman and meet consult. If it ever
// starts passing on an agent that cannot actually be steered, every "steering:
// true" in the fleet becomes a lie at once.
func TestSteerableDefersToChat(t *testing.T) {
	s := &Session{state: State{Agent: "definitely-not-a-registered-tool"}}
	ok, _ := s.steerable()
	if ok {
		t.Fatal("steerable() said yes to an agent chat.CanSteer rejects")
	}
	if chatOK, _ := chat.CanSteer("definitely-not-a-registered-tool"); chatOK {
		t.Fatal("chat.CanSteer said yes to an unregistered agent")
	}
}
