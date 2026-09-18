package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/admission"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/llmbudget"
)

// A live session is a CONVERSATION, and every steer that lands on Say buys
// another turn against the same model. Start gates only the opening prompt, so
// before this a coached run could spend without limit past an exhausted budget:
// the gate was asked once and never again.
//
// Both cases inject the clock and the counters. Nothing sleeps, nothing dials a
// network, and no model is ever called.

// budgetGate returns a gate holding `spent` dollars already burned today
// against a $10/day api-key model, with a frozen clock.
func budgetGate(t *testing.T, spent float64) func() {
	return budgetGateFor(t, "acme-1", spent)
}

func budgetGateFor(t *testing.T, model string, spent float64) func() {
	t.Helper()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	g := llmbudget.New(llmbudget.Config{
		Now: func() time.Time { return now },
		Models: map[string]llmbudget.Model{
			model: {
				Name:      model,
				Kind:      "api",
				Billing:   "metered",
				Provider:  "acme",
				CostMicro: 1000, // $0.001/token — a 100-token steer costs $0.10
				Limits:    llmbudget.Limits{BudgetUSD: 10},
			},
		},
	})
	// Counters carry a day boundary; stamp it the way the gate computes it so the
	// seeded spend is not rolled over as stale.
	y, m, d := now.Local().Date()
	g.Load(llmbudget.State{Models: map[string]llmbudget.Counters{
		model: {
			DayStart:   time.Date(y, m, d, 0, 0, 0, 0, now.Local().Location()),
			WeekStart:  weekStartForTest(now),
			DayCostUSD: spent,
		},
	}})
	return llmbudget.SetDefault(g)
}

func weekStartForTest(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, t.Local().Location())
	wd := int(day.Weekday())
	if wd == 0 {
		wd = 7
	}
	return day.AddDate(0, 0, -(wd - 1))
}

// sessionUnderTest is a session that never launched a process. Say must reach
// its verdict from the budget alone, BEFORE touching the control channel — so
// an exhausted budget is proven to stop the turn rather than merely failing on
// I/O afterwards.
func sessionUnderTest() *Session {
	return &Session{
		Agent:  "acme:acme-1",
		Nick:   "acme",
		launch: Launch{ModelName: "acme-1", Nick: "acme", ToolName: "acme"},
		done:   make(chan struct{}),
	}
}

func TestSayIsRefusedWhenTheBudgetIsExhausted(t *testing.T) {
	defer budgetGate(t, 9.99)() // $9.99 of a $10 cap already spent

	s := sessionUnderTest()
	err := s.Say(strings.Repeat("steer the agent ", 40)) // ~160 tokens ≈ $0.16

	if err == nil {
		t.Fatal("a steer past an exhausted budget was allowed; the gate covers only the opening prompt")
	}
	if !strings.Contains(err.Error(), "LLM budget") {
		t.Fatalf("refused for the wrong reason, want an LLM budget refusal, got: %v", err)
	}
}

func TestRefusedSayDoesNotAcknowledgePreparedInbox(t *testing.T) {
	isolateRoom(t)
	defer budgetGate(t, 9.99)()
	if err := bus.Publish(bus.Notification{Principal: "owner", To: "budget-agent", Body: "do not consume me"}); err != nil {
		t.Fatal(err)
	}

	s := sessionUnderTest()
	s.inboxAgent = "budget-agent"
	if err := s.Say(strings.Repeat("steer the agent ", 40)); err == nil || !strings.Contains(err.Error(), "LLM budget") {
		t.Fatalf("want budget refusal, got %v", err)
	}
	snapshot, err := bus.SnapshotInbox("budget-agent")
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Body != "do not consume me" {
		t.Fatalf("refused launch consumed inbox: items=%+v err=%v", snapshot.Items, err)
	}
}

func TestRefusedChatLaunchDoesNotAcknowledgePreparedInbox(t *testing.T) {
	isolateRoom(t)
	permitUnsafeLaunch(t)
	pinCatalog(t)
	defer budgetGateFor(t, "opus5", 9.99)()

	acked := false
	old := bus.PrepareTurnItems
	bus.PrepareTurnItems = func(string) ([]admission.Item, error) {
		return []admission.Item{{
			Source: "meet", ID: "launch-1", Priority: admission.PriorityResponse,
			Body: "waiting before launch", ArtifactRef: "bashy inbox --id meet:launch-1 --peek",
			OverflowRef: "bashy inbox --peek",
			Acknowledge: func() error { acked = true; return nil },
		}}, nil
	}
	t.Cleanup(func() { bus.PrepareTurnItems = old })

	_, err := Start(context.Background(), "claude:opus", SessionOptions{Prompt: strings.Repeat("launch prompt ", 40)})
	if err == nil || !strings.Contains(err.Error(), "LLM budget") {
		t.Fatalf("want budget refusal, got %v", err)
	}
	if acked {
		t.Fatal("refused chat launch acknowledged prepared inbox")
	}
}

func TestSayIsAllowedWhileBudgetRemains(t *testing.T) {
	defer budgetGate(t, 0)()

	s := sessionUnderTest()
	err := s.Say("carry on")

	// The gate allows, so Say proceeds to the control channel — which this
	// session does not have. That specific error is the proof it got PAST the
	// budget rather than being stopped by it.
	if err == nil || !strings.Contains(err.Error(), "no control channel") {
		t.Fatalf("want the send to pass the gate and fail on the missing control channel, got: %v", err)
	}
}

// Quit must never be gated: an exhausted budget still has to be able to shut a
// running session down, or the refusal path leaks processes.
func TestQuitIsNotGated(t *testing.T) {
	defer budgetGate(t, 1e6)() // hopelessly over

	s := sessionUnderTest()
	if err := s.Quit(); err == nil || !strings.Contains(err.Error(), "no control channel") {
		t.Fatalf("Quit must bypass the budget gate, got: %v", err)
	}
}
