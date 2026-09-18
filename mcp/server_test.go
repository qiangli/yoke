package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/coreutils/tool"
)

func init() {
	// A throwaway tool that echoes stdin uppercased-ish so run_tool's
	// stdio + exit mapping is observable.
	tool.Register(&tool.Tool{
		Name:     "mcpprobe",
		Synopsis: "mcp test probe",
		Usage:    "mcpprobe [fail]",
		Run: func(rc *tool.RunContext, args []string) int {
			buf := make([]byte, 0, 64)
			tmp := make([]byte, 32)
			for {
				n, err := rc.In.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			rc.Out.Write([]byte("out:" + strings.TrimSpace(string(buf)) + ":" + strings.Join(args, ",")))
			if len(args) > 0 && args[0] == "fail" {
				rc.Err.Write([]byte("boom"))
				return 5
			}
			return 0
		},
	})
}

// connect spins up an in-memory server+client pair against the registry.
func connect(t *testing.T) (*mcpsdk.ClientSession, context.Context) {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	srv := NewServer("coreutils", "test")
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, ctx
}

func decodeStructured[T any](t *testing.T, res *mcpsdk.CallToolResult) T {
	t.Helper()
	var v T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal structured into %T: %v", v, err)
	}
	return v
}

func TestListTools(t *testing.T) {
	cs, ctx := connect(t)
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "list_tools", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call list_tools: %v", err)
	}
	out := decodeStructured[ListToolsOutput](t, res)
	var found bool
	for _, ti := range out.Tools {
		if ti.Name == "mcpprobe" {
			found = true
			if ti.Synopsis != "mcp test probe" {
				t.Errorf("synopsis = %q", ti.Synopsis)
			}
			// mcpprobe has no Command Atlas entry: the additive atlas
			// fields must be absent, not fabricated.
			if ti.Group != "" || len(ti.Caps) != 0 {
				t.Errorf("atlas fields for unlisted tool: group=%q caps=%v", ti.Group, ti.Caps)
			}
		}
	}
	if !found {
		t.Fatalf("mcpprobe not listed; got %d tools", len(out.Tools))
	}
}

func TestRunToolSuccess(t *testing.T) {
	cs, ctx := connect(t)
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "run_tool",
		Arguments: RunToolInput{
			Name:  "mcpprobe",
			Args:  []string{"x", "y"},
			Stdin: "hello\n",
		},
	})
	if err != nil {
		t.Fatalf("call run_tool: %v", err)
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", out.ExitCode)
	}
	if out.Stdout != "out:hello:x,y" {
		t.Errorf("stdout = %q", out.Stdout)
	}
}

func TestRunToolNonzeroExitAndStderr(t *testing.T) {
	cs, ctx := connect(t)
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "run_tool",
		Arguments: RunToolInput{Name: "mcpprobe", Args: []string{"fail"}, Stdin: "z"},
	})
	if err != nil {
		t.Fatalf("call run_tool: %v", err)
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 5 {
		t.Errorf("exit = %d, want 5", out.ExitCode)
	}
	if out.Stderr != "boom" {
		t.Errorf("stderr = %q", out.Stderr)
	}
}

func TestRunToolUnknown(t *testing.T) {
	cs, ctx := connect(t)
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "run_tool",
		Arguments: RunToolInput{Name: "definitely-not-a-tool"},
	})
	if err != nil {
		t.Fatalf("call run_tool: %v", err)
	}
	out := decodeStructured[RunToolOutput](t, res)
	if out.ExitCode != 2 {
		t.Errorf("exit = %d, want 2 (unknown tool)", out.ExitCode)
	}
	if !strings.Contains(out.Stderr, "not a supported command") {
		t.Errorf("stderr missing diagnostic: %q", out.Stderr)
	}
}
