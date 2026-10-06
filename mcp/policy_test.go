package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/qiangli/coreutils/cmds/echo"
	_ "github.com/qiangli/coreutils/cmds/rm"
	"github.com/qiangli/yoke/pkg/atlas"
)

func policyClient(t *testing.T, srv *mcpsdk.Server) (*mcpsdk.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "policy-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, ctx
}

func TestPolicyCalls(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		allow         bool
		code          int
		denial        string
	}{
		{"rm denied", "rm", false, 126, "denied: effect destroy requires --allow destroy"},
		{"rm allowed", "rm", true, 0, ""},
		{"echo allowed", "echo", false, 0, ""},
		{"unknown", "mcp-no-such-command", false, 2, "unknown command: mcp-no-such-command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := make(chan Record, 2)
			p := &Policy{Allow: map[string]bool{"destroy": tc.allow}, Audit: func(r Record) { records <- r }}
			cs, ctx := policyClient(t, NewServerWithOptions("policy", "test", Options{Policy: p}))
			path := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(path, []byte("keep until allowed"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"hello"}
			if tc.command == "rm" {
				args = []string{path}
			}
			res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: tc.command, Args: args}})
			if err != nil {
				t.Fatal(err)
			}
			out := decodeStructured[RunToolOutput](t, res)
			if out.ExitCode != tc.code || res.IsError != (tc.denial != "") {
				t.Fatalf("result = %+v, output = %+v", res, out)
			}
			if tc.denial != "" {
				if len(res.Content) != 1 {
					t.Fatalf("content = %v", res.Content)
				}
				txt, ok := res.Content[0].(*mcpsdk.TextContent)
				if !ok || txt.Text != tc.denial {
					t.Fatalf("content = %+v", res.Content[0])
				}
			}
			if tc.command == "echo" && out.Stdout != "hello\n" {
				t.Fatalf("stdout = %q", out.Stdout)
			}
			if tc.command == "rm" {
				_, err := os.Stat(path)
				if tc.allow && !os.IsNotExist(err) {
					t.Fatalf("allowed rm did not remove file: %v", err)
				}
				if !tc.allow && err != nil {
					t.Fatalf("denied rm modified file: %v", err)
				}
			}
			select {
			case r := <-records:
				if r.Tool != "run_tool" || r.Command != tc.command || r.Allowed != (tc.denial == "") || r.ExitCode != tc.code || r.Denial != tc.denial || r.Duration <= 0 {
					t.Fatalf("audit = %+v", r)
				}
				if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
					t.Fatal(err)
				}
				if tc.command == "rm" && !strings.Contains(strings.Join(r.Effects, ","), "destroy") {
					t.Fatalf("effects = %v", r.Effects)
				}
			case <-ctx.Done():
				t.Fatal("missing audit")
			}
			select {
			case r := <-records:
				t.Fatalf("duplicate audit: %+v", r)
			default:
			}
		})
	}
}

func TestPolicyCheckOrder(t *testing.T) {
	p := &Policy{}
	effects := []string{"spend", "priv", "destroy", "cred"}
	for _, effect := range atlas.Effects() {
		if !privileged(effect) {
			continue
		}
		want := "denied: effect " + effect + " requires --allow " + effect
		if err := p.Check("rm", effects); err == nil || err.Error() != want {
			t.Fatalf("Check = %v, want %q", err, want)
		}
		if p.Allow == nil {
			p.Allow = map[string]bool{}
		}
		p.Allow[effect] = true
	}
	if err := p.Check("rm", effects); err != nil {
		t.Fatal(err)
	}
	if err := p.Check("mcp-no-such-command", nil); err == nil || err.Error() != "unknown command: mcp-no-such-command" {
		t.Fatalf("unknown = %v", err)
	}
	if err := (*Policy)(nil).Check("rm", []string{"destroy"}); err == nil {
		t.Fatal("nil policy allowed destroy")
	}
}

func TestParseAllow(t *testing.T) {
	for _, input := range []string{"network", "destroy,network", "pure", "destroy,"} {
		if _, err := ParseAllow(input); err == nil {
			t.Fatalf("accepted %q", input)
		} else {
			for _, word := range []string{"destroy", "spend", "cred", "priv"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("error omits %s: %v", word, err)
				}
			}
		}
	}
	grants, err := ParseAllow(" destroy,spend,cred,priv,destroy ")
	if err != nil || len(grants) != 4 {
		t.Fatalf("grants = %v, err = %v", grants, err)
	}
	grants, err = ParseAllow("")
	if err != nil || len(grants) != 0 {
		t.Fatalf("empty = %v, %v", grants, err)
	}
}

func TestPolicyHashAndServerInfo(t *testing.T) {
	records := make(chan Record, 4)
	a := NewServerWithOptions("policy", "test", Options{Policy: &Policy{Allow: map[string]bool{"destroy": true}, Audit: func(r Record) { records <- r }}})
	b := NewServer("policy", "test")
	cs, ctx := policyClient(t, a)
	h1, err := ToolsSHA256(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := ToolsSHA256(ctx, b)
	if err != nil || h1 != h2 || len(h1) != 64 {
		t.Fatalf("hashes = %q, %q, err %v", h1, h2, err)
	}
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(listed)
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(generic)
	if want := fmt.Sprintf("%x", sha256.Sum256(canonical)); h1 != want {
		t.Fatalf("hash = %s, canonical tools/list = %s JSON %s", h1, want, canonical)
	}
	b.AddTool(&mcpsdk.Tool{Name: "dummy", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	h2, err = ToolsSHA256(ctx, b)
	if err != nil || h1 == h2 {
		t.Fatalf("extra tool hash = %q, err %v", h2, err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "server_info", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("server_info = %+v, %v", res, err)
	}
	info := decodeStructured[serverInfoOutput](t, res)
	if info.Name != "policy" || info.Version != "test" || info.ProtocolVersion != mcpsdk.SupportedProtocolVersions()[0] || info.ToolsSHA256 != h1 || strings.Join(info.AllowedEffects, ",") != "destroy" {
		t.Fatalf("info = %+v", info)
	}
	select {
	case r := <-records:
		if !r.Allowed || r.Tool != "server_info" || r.ExitCode != 0 {
			t.Fatalf("audit = %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("missing server_info audit")
	}
}

func TestPolicyAuditsOtherCalls(t *testing.T) {
	records := make(chan Record, 4)
	srv := NewServerWithOptions("policy", "test", Options{Policy: &Policy{Audit: func(r Record) { records <- r }}})
	called := false
	srv.AddTool(&mcpsdk.Tool{Name: "rm", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		called = true
		return &mcpsdk.CallToolResult{}, nil
	})
	cs, ctx := policyClient(t, srv)
	for _, tc := range []struct {
		name string
		args any
		code int
	}{
		{"list_tools", map[string]any{}, 0},
		{"rm", map[string]any{}, 126},
		{"run_tool", map[string]any{"name": 42}, 1},
		{"run_tool", RunToolInput{Name: "mcpprobe", Args: []string{"fail"}}, 5},
	} {
		_, _ = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		select {
		case r := <-records:
			if r.Tool != tc.name || r.ExitCode != tc.code {
				t.Fatalf("audit = %+v, want %s exit %d", r, tc.name, tc.code)
			}
		case <-ctx.Done():
			t.Fatal("missing audit")
		}
	}
	if called {
		t.Fatal("privileged direct tool dispatched")
	}
}

func TestPolicyDefaultAudit(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous; writer.Close() }()
	cs, ctx := policyClient(t, NewServer("policy", "test"))
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm"}})
	if err != nil || !res.IsError {
		t.Fatalf("default policy = %+v, %v", res, err)
	}
	os.Stderr = previous
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("audit is not one line: %q", data)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Allowed || record.ExitCode != 126 || record.Command != "rm" || record.Denial != "denied: effect destroy requires --allow destroy" {
		t.Fatalf("audit = %+v", record)
	}
}

func TestPolicyHashPagination(t *testing.T) {
	makeServer := func(pageSize int) *mcpsdk.Server {
		srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "pages", Version: "1"}, &mcpsdk.ServerOptions{PageSize: pageSize})
		for _, name := range []string{"a", "b", "c"} {
			srv.AddTool(&mcpsdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{}, nil
			})
		}
		return srv
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h1, err := ToolsSHA256(ctx, makeServer(1))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := ToolsSHA256(ctx, makeServer(100))
	if err != nil || h1 != h2 {
		t.Fatalf("pagination changed hash: %q, %q, %v", h1, h2, err)
	}
}
