package acp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
)

func raiseConfirm(t *testing.T, session string, argv ...string) ask.Request {
	t.Helper()
	r, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: argv, Session: session, Prompt: "run " + strings.Join(argv, " ")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hostSelecting(kind PermissionOptionKind) PermissionAsker {
	return func(_ context.Context, req PermissionRequest) (PermissionResponse, error) {
		for _, o := range req.Options {
			if o.Kind == kind {
				return PermissionResponse{OptionID: o.ID}, nil
			}
		}
		return PermissionResponse{Cancelled: true}, nil
	}
}

func TestRelayApprovalHostAllowOnceIsSingleUse(t *testing.T) {
	isolateQueue(t)
	argv := []string{"rm", "-rf", "build"}
	r := raiseConfirm(t, "card-1", argv...)
	d, ok := RelayApproval(context.Background(), r, hostSelecting(PermissionAllowOnce))
	if !ok || !d.Allowed() || d.Always || d.Via != ask.ViaACP || d.OptionID != RelayOptionAllowOnce {
		t.Fatalf("decision = %+v ok=%v, want allow-once via acp", d, ok)
	}
	if got, ok := ask.Resolve(ask.SourceConfirm, r.Approval.Token, "card-1", argv); !ok || !got.Allowed() {
		t.Fatalf("first resume = %+v ok=%v, want allow", got, ok)
	}
	if _, ok := ask.Resolve(ask.SourceConfirm, r.Approval.Token, "card-1", argv); ok {
		t.Fatal("a once-allow was redeemed twice")
	}
}

func TestRelayApprovalHostRejectReplaysAsDeny(t *testing.T) {
	isolateQueue(t)
	argv := []string{"git", "push", "--force"}
	r := raiseConfirm(t, "card-1", argv...)
	d, ok := RelayApproval(context.Background(), r, hostSelecting(PermissionRejectOnce))
	if !ok || d.Allowed() || d.Via != ask.ViaACP {
		t.Fatalf("decision = %+v ok=%v, want deny via acp", d, ok)
	}
	for i := 0; i < 2; i++ {
		if got, ok := ask.Resolve(ask.SourceConfirm, r.Approval.Token, "card-1", argv); !ok || got.Allowed() {
			t.Fatalf("replay %d = %+v ok=%v, want the deny again", i, got, ok)
		}
	}
}

func TestRelayApprovalUnofferedOptionIsDeny(t *testing.T) {
	isolateQueue(t)
	r := raiseConfirm(t, "card-1", "deploy")
	d, ok := RelayApproval(context.Background(), r, func(context.Context, PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{OptionID: "made-up"}, nil
	})
	if !ok || d.Allowed() {
		t.Fatalf("decision = %+v ok=%v, want deny", d, ok)
	}
}

func TestRelayApprovalCancelledOrUnreachableStaysPending(t *testing.T) {
	isolateQueue(t)
	r := raiseConfirm(t, "card-1", "deploy")
	for _, host := range []PermissionAsker{
		func(context.Context, PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Cancelled: true}, nil
		},
		func(context.Context, PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{}, errors.New("host gone")
		},
	} {
		if d, ok := RelayApproval(context.Background(), r, host); ok {
			t.Fatalf("decision = %+v, want none", d)
		}
	}
	if p, _ := ask.PendingForSession("card-1"); len(p) != 1 {
		t.Fatalf("pending = %d, want the approval left for bashy approve", len(p))
	}
}

// A `bashy approve` that lands first wins, and the host's open question is
// withdrawn rather than left blocking the turn.
func TestRelayApprovalDecidedElsewhereWithdrawsHostQuestion(t *testing.T) {
	isolateQueue(t)
	r := raiseConfirm(t, "card-1", "deploy")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = ask.Decide(r.ID, ask.Decision{Verdict: ask.VerdictDeny, By: "operator"}, ask.DecideOpts{})
	}()
	d, ok := RelayApproval(context.Background(), r, func(ctx context.Context, _ PermissionRequest) (PermissionResponse, error) {
		<-ctx.Done()
		return PermissionResponse{Cancelled: true}, nil
	})
	if !ok || d.Allowed() || d.By != "operator" {
		t.Fatalf("decision = %+v ok=%v, want the operator's deny", d, ok)
	}
}

func TestRelayApprovalsAsksEachApprovalOnce(t *testing.T) {
	isolateQueue(t)
	raiseConfirm(t, "card-1", "one")
	raiseConfirm(t, "other-card", "not-mine")
	var mu sync.Mutex
	var titles []string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go RelayApprovals(ctx, "card-1", func(_ context.Context, req PermissionRequest) (PermissionResponse, error) {
		mu.Lock()
		titles = append(titles, req.ToolCall.Title)
		mu.Unlock()
		return PermissionResponse{Cancelled: true}, nil // leaves it pending: must not be re-asked
	})
	time.Sleep(5 * RelayPoll)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 1 || titles[0] != "run one" {
		t.Fatalf("host asked %q, want exactly [run one]", titles)
	}
}

// --- process-level round trip -------------------------------------------------

const relayHelperEnv = "YOKE_ACP_RELAY_HELPER"

// TestRelayHelperProcess is the `bashy acp` side, run as a real subprocess: an
// ACP agent whose turn raises an approval in the shared queue (as the local
// session's @confirm would), relays it to the host, and reports the decision
// that stands.
func TestRelayHelperProcess(t *testing.T) {
	if os.Getenv(relayHelperEnv) != "1" {
		t.Skip("helper process")
	}
	runner := RunnerFunc(func(ctx context.Context, req TurnRequest) (TurnResponse, error) {
		session := "card-" + req.SessionID
		argv := []string{"deploy", promptTextForTest(req.Prompt)}
		r, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Argv: argv, Session: session, Prompt: "run " + strings.Join(argv, " ")})
		if err != nil {
			return TurnResponse{}, err
		}
		rctx, stop := context.WithCancel(ctx)
		defer stop()
		go RelayApprovals(rctx, session, req.RequestPermission)
		d, err := ask.WaitDecision(ctx, r.ID)
		if err != nil {
			return TurnResponse{}, err
		}
		return TurnResponse{Text: d.Verdict + " via " + d.Via, StopReason: StopReasonEndTurn}, nil
	})
	<-NewAgent(runner, AgentOptions{}, os.Stdin, os.Stdout).Done()
	os.Exit(0)
}

func promptTextForTest(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, " ")
}

type relayHost struct {
	RecordingHandler
	choose PermissionOptionKind
	mu     sync.Mutex
	asked  []PermissionRequest
}

func (h *relayHost) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionResponse, error) {
	h.mu.Lock()
	h.asked = append(h.asked, req)
	h.mu.Unlock()
	return hostSelecting(h.choose)(ctx, req)
}

func TestRelayProcessRoundTripAllowAndDeny(t *testing.T) {
	isolateQueue(t)
	for _, tc := range []struct {
		choose PermissionOptionKind
		want   string
	}{
		{PermissionAllowOnce, "allow via acp"},
		{PermissionRejectOnce, "deny via acp"},
	} {
		t.Run(string(tc.choose), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRelayHelperProcess$")
			cmd.Env = append(os.Environ(), relayHelperEnv+"=1")
			host := &relayHost{choose: tc.choose}
			c, err := NewClient(ctx, host, cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			cwd, _ := os.Getwd()
			sid, err := c.NewSession(ctx, cwd)
			if err != nil {
				t.Fatal(err)
			}
			stop, err := c.Prompt(ctx, sid, string(tc.choose))
			if err != nil || stop != StopReasonEndTurn {
				t.Fatalf("prompt = %q, %v", stop, err)
			}
			if got := host.AgentText(); got != tc.want {
				t.Fatalf("agent text = %q, want %q", got, tc.want)
			}
			host.mu.Lock()
			asked := host.asked
			host.mu.Unlock()
			if len(asked) != 1 || asked[0].SessionID != sid || !strings.HasPrefix(asked[0].ToolCall.ID, "approval:") ||
				asked[0].ToolCall.Title != "run deploy "+string(tc.choose) || len(asked[0].Options) != 3 {
				t.Fatalf("host was asked %+v", asked)
			}
			// The host's answer is on the record `bashy approve` reads.
			all, _ := ask.ListApprovals()
			var found bool
			for _, r := range all {
				if r.Approval.Session != "card-"+sid || r.Approval.Argv[1] != string(tc.choose) {
					continue
				}
				d, ok := ask.LoadDecision(r.ID)
				found = ok && d.Via == ask.ViaACP && d.By == "acp-host"
			}
			if !found {
				t.Fatal("no queue decision via acp recorded")
			}
		})
	}
}
