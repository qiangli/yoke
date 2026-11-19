package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"strings"
	"testing"
	"time"
)

func deliberationFixture(t *testing.T) (*State, DeliberationPlan) {
	st := newRoom(t)
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "panel-human")
	st.Human, st.Initiator = "panel-human", "panel-human"
	oldDetect := bus.DetectHarness
	bus.DetectHarness = nil
	t.Cleanup(func() { bus.DetectHarness = oldDetect })
	st.Participants = []string{"codex", "claude", "critic", "facilitator", "expert", "bystander"}
	if e := st.save(); e != nil {
		t.Fatal(e)
	}
	return st, DeliberationPlan{Question: "Choose a design", Proposers: []string{"codex", "claude"}, Experts: []string{"expert"}, Critic: "critic", Facilitator: "facilitator", MaxTurns: 8, Duration: time.Minute, TurnTimeout: time.Second}
}
func TestDeliberationWorkflow(t *testing.T) {
	st, p := deliberationFixture(t)
	var calls []string
	invoke := func(ctx context.Context, o chat.Options) (chat.Result, error) {
		calls = append(calls, o.Agent)
		if !o.ReadOnly || o.AllowUnsafe || o.Stream != nil || o.EventStream != nil {
			t.Fatal("authority/stream leak")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		reply := DeliberationReply{Text: "secret-" + o.Agent, Evidence: []DeliberationEvidence{{Source: "fixture:benchmark", Claim: "measured"}}}
		if len(calls) <= 2 {
			if strings.Contains(strings.Join(o.Context, ""), "secret-") {
				t.Fatal("peer leak")
			}
			events, _ := readTranscript(st.ID)
			for _, ev := range events {
				if strings.Contains(ev.Text, "secret-") {
					t.Fatal("published sealed reply")
				}
			}
		} else if !strings.Contains(strings.Join(o.Context, ""), "secret-codex") {
			t.Fatal("no reveal")
		}
		if o.Agent == "critic" {
			reply.Dissent = []string{"unresolved reliability risk"}
		}
		if len(calls) == 4 {
			reply.Request = &DeliberationEvidenceRequest{Expert: "expert", Question: "benchmark?"}
		}
		b, _ := json.Marshal(reply)
		return chat.Result{Output: string(b)}, nil
	}
	r, e := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Join(calls, ",") != "codex,claude,critic,facilitator,expert,facilitator" {
		t.Fatal(calls)
	}
	if r.Status != "proposal" || r.Stop != "concluded-proposal" || r.Turns != 6 || r.Contributions[2].Dissent[0] != "unresolved reliability risk" {
		t.Fatalf("%+v", r)
	}
	events, _ := readTranscript(st.ID)
	if events[1].Kind != "deliberation-reveal" || !strings.Contains(events[len(events)-1].Text, "unresolved reliability risk") {
		t.Fatal(events)
	}
	for _, ev := range events {
		if ev.Kind == "action" || ev.Kind == "decision" {
			t.Fatal("automatic adoption")
		}
	}
}
func TestDeliberationStops(t *testing.T) {
	for _, mode := range []string{"budget", "timeout", "failure", "invalid-target", "invalid-json"} {
		t.Run(mode, func(t *testing.T) {
			st, p := deliberationFixture(t)
			p.MaxTurns = 4
			calls := 0
			invoke := func(ctx context.Context, o chat.Options) (chat.Result, error) {
				calls++
				if mode == "failure" {
					return chat.Result{}, errors.New("failed")
				}
				if mode == "invalid-json" {
					return chat.Result{Output: "invalid"}, nil
				}
				if mode == "timeout" {
					<-ctx.Done()
					return chat.Result{}, ctx.Err()
				}
				reply := DeliberationReply{Text: "answer"}
				if o.Agent == "facilitator" {
					target := "expert"
					if mode == "invalid-target" {
						target = "bystander"
					}
					reply.Request = &DeliberationEvidenceRequest{Expert: target, Question: "evidence?"}
				}
				b, _ := json.Marshal(reply)
				return chat.Result{Output: string(b)}, nil
			}
			if mode == "timeout" {
				p.Duration = 10 * time.Millisecond
				p.TurnTimeout = p.Duration
			}
			r, e := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke)
			if e == nil || r.Conclusion != "" || calls > p.MaxTurns {
				t.Fatalf("%+v %v %d", r, e, calls)
			}
			if mode == "budget" && r.Stop != "turn-budget" {
				t.Fatal(r)
			}
			events, _ := readTranscript(st.ID)
			if events[len(events)-1].Kind != "deliberation-stop" {
				t.Fatal("no terminal record")
			}
			if calls < 2 && len(r.Contributions) != 0 {
				t.Fatal("partial reveal")
			}
		})
	}
}
func TestDeliberationAdmission(t *testing.T) {
	st, p := deliberationFixture(t)
	called := false
	invoke := func(context.Context, chat.Options) (chat.Result, error) { called = true; return chat.Result{}, nil }
	for _, actor := range []string{"outsider"} {
		if _, e := deliberateWith(context.Background(), st.ID, actor, p, invoke); e == nil {
			t.Fatal("unauthorized")
		}
	}
	p.Critic = p.Proposers[0]
	if _, e := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke); e == nil {
		t.Fatal("role overlap")
	}
	if called {
		t.Fatal("invalid launch")
	}
	cmd := NewMeetCmd()
	if c, _, e := cmd.Find([]string{"deliberate"}); e != nil || c.Name() != "deliberate" {
		t.Fatal("missing CLI")
	}
}

func TestDeliberationAuthenticatedOrganizer(t *testing.T) {
	st, p := deliberationFixture(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	store := fleet.NewInstanceStore(t.TempDir())
	oldStore, oldSession := bus.InstanceStoreFn, bus.CurrentSessionClaim
	bus.InstanceStoreFn = func() *fleet.InstanceStore { return store }
	bus.CurrentSessionClaim = func(string) string { return "deliberation-test-session" }
	t.Cleanup(func() { bus.InstanceStoreFn = oldStore; bus.CurrentSessionClaim = oldSession })
	inst, err := store.Open(fleet.Family{Name: "panel-organizer", Policy: fleet.PolicySingle, Bindings: []string{"codex"}}, fleet.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_PRINCIPAL", inst.URN())
	bus.DetectHarness = func() (string, bool) { return "codex", true }
	calls := 0
	invoke := func(context.Context, chat.Options) (chat.Result, error) {
		calls++
		return chat.Result{Output: `{"text":"proposal"}`}, nil
	}
	// Neither the API nor CLI may impersonate the human organizer.
	for _, as := range []string{st.Initiator, "another-organizer"} {
		if _, err := deliberateWith(context.Background(), st.ID, as, p, invoke); err == nil {
			t.Fatal("spoof accepted", as)
		}
		cmd := NewMeetCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"deliberate", st.ID, "--as", as})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot author as") {
			t.Fatalf("CLI spoof: %v", err)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized launch")
	}
	if events, _ := readTranscript(st.ID); len(events) != 0 {
		t.Fatal("unauthorized transcript", events)
	}
	st.Initiator = ""
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := deliberateWith(context.Background(), st.ID, "", p, invoke); !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("unnamed organizer: %v", err)
	}
	if calls != 0 {
		t.Fatal("unnamed organizer launched")
	}
	st.Initiator = inst.MailAddress()
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	for _, as := range []string{"", inst.UUID, inst.MailAddress()} {
		result, err := deliberateWith(context.Background(), st.ID, as, p, invoke)
		if err != nil || result.Stop != "concluded-proposal" {
			t.Fatalf("own instance: %+v %v", result, err)
		}
	}
	events, _ := readTranscript(st.ID)
	for _, ev := range events {
		if ev.Kind == "deliberation-start" && ev.Speaker != inst.MailAddress() {
			t.Fatal("wrong attribution", ev)
		}
	}
}

func TestDeliberationUnattributedHarnessHumanSpoof(t *testing.T) {
	st, p := deliberationFixture(t)
	bus.DetectHarness = func() (string, bool) { return "codex", true }
	// The authored helper accepts this account-resolved person. Assert that
	// precondition so this regression specifically exercises the reported hole.
	actor, err := bus.ResolveAuthoredActor(st.Initiator)
	if err != nil || actor != st.Initiator {
		t.Fatalf("human resolution: %q %v", actor, err)
	}
	calls := 0
	invoke := func(context.Context, chat.Options) (chat.Result, error) {
		calls++
		return chat.Result{Output: `{"text":"proposal"}`}, nil
	}
	if _, err := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke); err == nil {
		t.Fatal("unattributed harness impersonated human")
	}
	cmd := NewMeetCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deliberate", st.ID, "--as", st.Initiator})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot deliberate as a human") {
		t.Fatalf("CLI spoof: %v", err)
	}
	if calls != 0 {
		t.Fatal("unauthorized panel")
	}
	if events, err := readTranscript(st.ID); err != nil || len(events) != 0 {
		t.Fatalf("unauthorized transcript: %v %v", events, err)
	}
	// Outside an agent harness, only the account-bound human can organize.
	bus.DetectHarness = func() (string, bool) { return "", false }
	t.Setenv("USER", "different-human")
	if _, err := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke); err == nil {
		t.Fatal("other account impersonated organizer")
	}
	if calls != 0 {
		t.Fatal("unauthorized account launched panel")
	}
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", st.Initiator)
	result, err := deliberateWith(context.Background(), st.ID, st.Initiator, p, invoke)
	if err != nil || result.Stop != "concluded-proposal" {
		t.Fatalf("human organizer: %+v %v", result, err)
	}
}
