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

// A gated call with a raise hook returns needs-approval with the same
// approval event shape the pull route emits, and the digest binds the exact
// candidate the hook saw.
func TestNeedsApprovalShape(t *testing.T) {
	isolateAsk(t)
	records := make(chan Record, 2)
	hook := GatewayApprovalHook("hostA", "task-1")
	p := &Policy{Audit: func(r Record) { records <- r }, RaiseApproval: hook}
	cs, ctx := policyClient(t, NewServerWithOptions("policy", "test", Options{Policy: p}))
	path := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(path, []byte("keep until approved"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm", Args: []string{path}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("gated call must not succeed without an approval")
	}
	got := decodeStructured[NeedsApproval](t, res)
	if got.State != NeedsApprovalState || got.ApprovalID == "" || got.Summary == "" || len(got.Effects) == 0 {
		t.Fatalf("needs-approval = %+v", got)
	}
	wantDigest := ask.GatewayDigest([]string{"rm", path}, "hostA", "task-1")
	if got.Digest != wantDigest {
		t.Fatalf("digest = %s, want %s", got.Digest, wantDigest)
	}
	if !strings.Contains(strings.Join(got.Effects, ","), "destroy") {
		t.Fatalf("effects = %v", got.Effects)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("gated rm executed without an approval: %v", err)
	}
	select {
	case r := <-records:
		if r.Allowed || r.Denial != "" || r.ApprovalID != got.ApprovalID || r.ExitCode != 126 {
			t.Fatalf("audit = %+v", r)
		}
	default:
		t.Fatal("missing audit")
	}
	// The returned digest settles through ApplyRemote; a stale one fails.
	rc, err := ask.ApplyRemote(ask.RemoteDirective{
		ID: "dir-1", Approval: got.ApprovalID, Host: "hostA",
		Token: got.Digest, Verdict: ask.VerdictAllow,
		By: "person:qiangli@hostB",
	}, ask.RemotePolicy{Approvers: []string{"qiangli"}})
	if err != nil || !rc.Applied() {
		t.Fatalf("matching digest did not settle: rc=%+v err=%v", rc, err)
	}
	rc, err = ask.ApplyRemote(ask.RemoteDirective{
		ID: "dir-2", Approval: got.ApprovalID, Host: "hostA",
		Token: strings.Repeat("0", 64), Verdict: ask.VerdictAllow,
		By: "person:qiangli@hostB",
	}, ask.RemotePolicy{Approvers: []string{"qiangli"}})
	if err == nil || rc.Applied() {
		t.Fatalf("stale digest settled: rc=%+v err=%v", rc, err)
	}
}

// A hook that declines, and an unknown command, both fail closed to the
// outright denial.
func TestNeedsApprovalFailsClosed(t *testing.T) {
	isolateAsk(t)
	called := false
	p := &Policy{RaiseApproval: func(GatedCall) (GatedGrant, bool) {
		called = true
		return GatedGrant{}, false
	}}
	cs, ctx := policyClient(t, NewServerWithOptions("policy", "test", Options{Policy: p}))
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm", Args: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("declined hook must deny")
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 126 || out.Stderr != "denied: effect destroy requires --allow destroy" {
		t.Fatalf("output = %+v", out)
	}
	if !called {
		t.Fatal("hook was not consulted")
	}
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "mcp-no-such-command"}})
	if err != nil || !res.IsError {
		t.Fatalf("unknown = %+v, %v", res, err)
	}
	out = decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 2 {
		t.Fatalf("unknown command lost its status: %+v", out)
	}
}

// Without a hook the historical outright denial is unchanged.
func TestNeedsApprovalNilHookDenies(t *testing.T) {
	isolateAsk(t)
	cs, ctx := policyClient(t, NewServer("policy", "test"))
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm", Args: []string{"x"}}})
	if err != nil || !res.IsError {
		t.Fatalf("result = %+v, %v", res, err)
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 126 {
		t.Fatalf("output = %+v", out)
	}
}
