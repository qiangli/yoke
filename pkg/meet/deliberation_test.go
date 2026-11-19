package meet

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/qiangli/yoke/pkg/chat"
	"strings"
	"testing"
	"time"
)

func deliberationFixture(t *testing.T) (*State, DeliberationPlan) {
	st := newRoom(t)
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
	for _, actor := range []string{"outsider", ""} {
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
