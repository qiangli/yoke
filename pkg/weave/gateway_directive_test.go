package weave

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

type gatewayTestClient struct {
	*directiveMockSessionClient
	mu         sync.Mutex
	events     []AppendEventReq
	failResult bool
}

func (c *gatewayTestClient) AppendEvent(_ context.Context, _ string, req AppendEventReq) (Event, error) {
	c.mu.Lock()
	if req.Kind == "result" && c.failResult {
		c.mu.Unlock()
		return Event{}, errors.New("temporary write failure")
	}
	c.events = append(c.events, req)
	c.mu.Unlock()
	return Event{}, nil
}

func (c *gatewayTestClient) snapshot() []AppendEventReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AppendEventReq(nil), c.events...)
}

func gatewayEvent(t *testing.T, id, task, key, verb string) Event {
	t.Helper()
	d := gatewayDirective{Verb: verb, TaskID: task, IdempotencyKey: key, Host: sessionHostName()}
	d.Args.Argv = []string{"ls"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return Event{ID: id, Kind: "directive", Detail: b}
}

func waitGatewayState(t *testing.T, g *gatewayConductor, key, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r, err := readGatewayRecord(gatewayRecordPath(g.dir, key))
		if err == nil && r.State == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("state = %q, want %q", r.State, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestGatewayDuplicateKeyKeepsFirstTask(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, t.TempDir())
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	g.execute = func(ctx context.Context, d gatewayDirective) (string, int, bool) {
		started <- struct{}{}
		<-release
		return "ok", 0, true
	}
	if err := g.apply(context.Background(), "task-a", gatewayEvent(t, "1", "task-a", "same", "run")); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := g.apply(context.Background(), "task-b", gatewayEvent(t, "2", "task-b", "same", "run")); err != nil {
		t.Fatal(err)
	}
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, "same"))
	if err != nil {
		t.Fatal(err)
	}
	if r.TaskID != "task-a" {
		t.Fatalf("task id = %q", r.TaskID)
	}
	select {
	case <-started:
		t.Fatal("duplicate executed")
	default:
	}
	close(release)
	g.wg.Wait()
	waitGatewayState(t, g, "same", "done")
}

func TestGatewayCancelRaceSuppressesResult(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, t.TempDir())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	g.execute = func(ctx context.Context, d gatewayDirective) (string, int, bool) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return "claimed success", 0, false
	}
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "key", "delegate")); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "2", "task", "key", "cancel")); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	close(release)
	g.wg.Wait()
	waitGatewayState(t, g, "key", "cancelled")
	for _, ev := range c.snapshot() {
		if ev.Kind == "result" {
			t.Fatal("cancelled task emitted result")
		}
	}
}

func TestGatewayCancelBeforeRunReservesKey(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, t.TempDir())
	g.execute = func(context.Context, gatewayDirective) (string, int, bool) {
		t.Fatal("cancelled task executed")
		return "", 0, false
	}
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "cancel", "task", "key", "cancel")); err != nil {
		t.Fatal(err)
	}
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "run", "task", "key", "run")); err != nil {
		t.Fatal(err)
	}
	waitGatewayState(t, g, "key", "cancelled")
}

func TestGatewayResultEventsMarkVerification(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, t.TempDir())
	g.execute = func(context.Context, gatewayDirective) (string, int, bool) { return "worker says done", 0, false }
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "key", "delegate")); err != nil {
		t.Fatal(err)
	}
	waitGatewayState(t, g, "key", "done")
	g.wg.Wait()
	kinds := []string{}
	for _, ev := range c.snapshot() {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "result" {
			var result struct {
				Text     string `json:"text"`
				Exit     int    `json:"exit"`
				Verified bool   `json:"verified"`
			}
			if err := json.Unmarshal(ev.Detail, &result); err != nil {
				t.Fatal(err)
			}
			if result.Text != "worker says done" || result.Exit != 0 || result.Verified {
				t.Fatalf("result = %+v", result)
			}
		}
	}
	if len(kinds) != 5 || kinds[0] != "ack" || kinds[1] != "state" || kinds[2] != "state" || kinds[3] != "result" || kinds[4] != "state" {
		t.Fatalf("event kinds = %v", kinds)
	}
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "late-cancel", "task", "key", "cancel")); err != nil {
		t.Fatal(err)
	}
	waitGatewayState(t, g, "key", "done")
	for _, ev := range c.snapshot()[len(kinds):] {
		if ev.Kind != "ack" {
			t.Fatal("late cancel changed a completed task")
		}
	}
}

func TestSubscriptionWorkerEnvExcludesMeteredCredentials(t *testing.T) {
	got := chat.ScrubDelegateEnv([]string{
		"OPENAI_API_KEY=masked", "ANTHROPIC_API_KEY=masked",
		"OTHER_AUTH_TOKEN=masked", "AWS_REGION=masked", "ANTHROPIC_BASE_URL=masked",
		"CLAUDE_CODE_OAUTH_TOKEN=opaque", "PATH=/bin",
	})
	if len(got) != 2 || got[0] != "CLAUDE_CODE_OAUTH_TOKEN=opaque" || got[1] != "PATH=/bin" {
		t.Fatalf("unexpected environment variable names after filtering")
	}
}

func TestGatewayRunEffectGate(t *testing.T) {
	for _, tc := range []struct {
		name, verb string
		runs       bool
	}{
		{"read", "ls", true},
		{"destroy", "rm", false},
		{"unknown", "not-an-atlas-verb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
			g := newGatewayConductor(c, t.TempDir())
			runs := 0
			g.execute = func(context.Context, gatewayDirective) (string, int, bool) {
				runs++
				return "ok", 0, true
			}
			ev := gatewayEvent(t, "1", "task", "key", "run")
			var d gatewayDirective
			if err := json.Unmarshal(ev.Detail, &d); err != nil {
				t.Fatal(err)
			}
			d.Args.Argv = []string{tc.verb}
			ev.Detail, _ = json.Marshal(d)
			if err := g.apply(context.Background(), "task", ev); err != nil {
				t.Fatal(err)
			}
			g.wg.Wait()
			if (runs == 1) != tc.runs {
				t.Fatalf("execute calls = %d", runs)
			}
			r, err := readGatewayRecord(gatewayRecordPath(g.dir, "key"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.runs && r.State != "done" {
				t.Fatalf("state = %s", r.State)
			}
			if !tc.runs && (r.State != "failed" || r.Reason != "gated-effect") {
				t.Fatalf("record = %+v", r)
			}
		})
	}
}

func TestGatewaySubscriptionArgs(t *testing.T) {
	if len(chat.DelegateSubscriptionArgs("claude")) == 0 || len(chat.DelegateSubscriptionArgs("codex")) == 0 {
		t.Fatal("subscription login arguments missing")
	}
}

func TestGatewayAckUsesDirectiveSequence(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, t.TempDir())
	ev := gatewayEvent(t, "event-id", "task", "key", "say")
	ev.Seq = 42
	if err := g.apply(context.Background(), "task", ev); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		DirectiveSeq int64 `json:"directive_seq"`
	}
	if err := json.Unmarshal(c.snapshot()[0].Detail, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.DirectiveSeq != 42 {
		t.Fatalf("directive_seq = %d", ack.DirectiveSeq)
	}
}

func TestGatewayResultRetriesAfterWriteFailure(t *testing.T) {
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}, failResult: true}
	g := newGatewayConductor(c, t.TempDir())
	g.execute = func(context.Context, gatewayDirective) (string, int, bool) { return "done", 0, true }
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "key", "run")); err != nil {
		t.Fatal(err)
	}
	g.wg.Wait()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Result == nil || r.ResultSent {
		t.Fatalf("result was not left pending: %+v", r)
	}
	c.mu.Lock()
	c.failResult = false
	c.mu.Unlock()
	if err := g.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err = readGatewayRecord(gatewayRecordPath(g.dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.ResultSent || !r.TerminalStateSent {
		t.Fatalf("outbox did not flush: %+v", r)
	}
}
