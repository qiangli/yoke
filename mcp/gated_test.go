package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/pkg/ask"
)

func isolateAsk(t *testing.T) {
	t.Helper()
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

func pendingApprovalCount(t *testing.T) int {
	t.Helper()
	pending, err := ask.PendingApprovals()
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

// A gated run_tool call answers the pull-route handoff, never executes,
// and raises no approval on the sync route: the digest-bound approval is
// the pull conductor's job, after the master re-dispatches the same call
// with the same idempotency key.
func TestGatedCallReturnsPullHandoff(t *testing.T) {
	isolateAsk(t)
	records := make(chan Record, 2)
	p := &Policy{Audit: func(r Record) { records <- r }}
	cs, ctx := policyClient(t, NewServerWithOptions("policy", "test", Options{Policy: p}))
	path := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(path, []byte("keep until approved on the pull route"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm", Args: []string{path}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("gated call must not succeed without an approval")
	}
	got := decodeStructured[Gated](t, res)
	if got.State != GatedState {
		t.Fatalf("state = %q, want %q", got.State, GatedState)
	}
	if !strings.Contains(strings.Join(got.Effects, ","), "destroy") {
		t.Fatalf("effects = %v, want destroy", got.Effects)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %v", res.Content)
	}
	if txt, ok := res.Content[0].(*mcpsdk.TextContent); !ok || !strings.HasPrefix(txt.Text, "gated:") {
		t.Fatalf("text = %+v", res.Content[0])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("gated rm executed without an approval: %v", err)
	}
	if n := pendingApprovalCount(t); n != 0 {
		t.Fatalf("sync route raised %d approvals, want none", n)
	}
	select {
	case r := <-records:
		if r.Allowed || r.Denial == "" || r.ApprovalID != "" || r.ExitCode != 126 {
			t.Fatalf("audit = %+v", r)
		}
	default:
		t.Fatal("missing audit")
	}
}

// A gated direct-tool call answers the same handoff shape as run_tool.
func TestGatedDirectCallReturnsPullHandoff(t *testing.T) {
	isolateAsk(t)
	policy := &Policy{Audit: func(Record) {}}
	srv := NewServerWithOptions("direct", "test", Options{Policy: policy})
	if err := registerRegistryTools(srv, policy, []string{"rm"}); err != nil {
		t.Fatal(err)
	}
	cs, ctx := policyClient(t, srv)
	path := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(path, []byte("keep until approved on the pull route"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "rm", Arguments: map[string]any{"args": []string{path}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("gated call must not succeed without an approval")
	}
	got := decodeStructured[Gated](t, res)
	if got.State != GatedState || !strings.Contains(strings.Join(got.Effects, ","), "destroy") {
		t.Fatalf("gated = %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("gated rm executed without an approval: %v", err)
	}
	if n := pendingApprovalCount(t); n != 0 {
		t.Fatalf("sync route raised %d approvals, want none", n)
	}
}

// An unknown command still denies outright: there is no known effect set
// to gate, so there is nothing for the pull route to approve.
func TestGatedUnknownCommandDenies(t *testing.T) {
	isolateAsk(t)
	cs, ctx := policyClient(t, NewServer("policy", "test"))
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "mcp-no-such-command"}})
	if err != nil || !res.IsError {
		t.Fatalf("result = %+v, %v", res, err)
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 2 {
		t.Fatalf("unknown command lost its status: %+v", out)
	}
	if n := pendingApprovalCount(t); n != 0 {
		t.Fatalf("sync route raised %d approvals, want none", n)
	}
}
