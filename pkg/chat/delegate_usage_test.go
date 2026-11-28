package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/yoke/mcp"
)

func TestReportedUsageIsNeverGuessed(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want *Usage
	}{
		"prose":           {"all done, usage was low\n", nil},
		"anthropic":       {`{"type":"result","result":"ok","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":5}}`, &Usage{100, 5}},
		"openai":          {`{"usage":{"prompt_tokens":12,"completion_tokens":3}}`, &Usage{12, 3}},
		"last json line":  {"working\n{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n{\"usage\":{\"input_tokens\":7,\"output_tokens\":8}}\n", &Usage{7, 8}},
		"usage no tokens": {`{"usage":{"note":"n/a"}}`, nil},
		"half a count":    {`{"usage":{"input_tokens":4}}`, nil},
	} {
		got := reportedUsage(tc.out)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: got %+v want %+v", name, got, tc.want)
		}
	}
}

// The typed tool `agent` wraps one read-only `delegate --json` turn of a fake
// tool and carries the usage that tool reported.
func TestAgentTypedToolWrapsDelegateAndReportsUsage(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(delegateDepthEnv, "")
	t.Setenv("BASHY_DELEGATE_ALLOW", "")
	t.Setenv("BASHY_DELEGATE_CALLER", "")
	fake := &fakeRunner{output: `{"type":"result","result":"reviewed","usage":{"input_tokens":21,"output_tokens":9}}` + "\n"}
	prev := delegateRunner
	delegateRunner = fake
	t.Cleanup(func() { delegateRunner = prev })

	ctx := context.Background()
	build := func() *mcpsdk.Server {
		srv, err := mcp.BuildServer("test", "0", mcp.Options{
			Policy:     &mcp.Policy{Allow: map[string]bool{"spend": true}},
			Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{MCPAgentCommand()} },
		})
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	srv := build()
	first, err := mcp.ToolsSHA256(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := mcp.ToolsSHA256(ctx, build()); first == "" || first != second {
		t.Fatalf("digest not stable: %q %q", first, second)
	}
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var def *mcpsdk.Tool
	for _, tl := range listed.Tools {
		if tl.Name == "agent" {
			def = tl
		}
	}
	if def == nil {
		t.Fatal("agent is not in the tool list")
	}
	raw, _ := json.Marshal(def.InputSchema)
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	_ = json.Unmarshal(raw, &schema)
	if schema.Properties["instruction"]["type"] != "string" || schema.Properties["max-tokens"]["type"] != "integer" || len(schema.Required) != 2 {
		t.Fatalf("agent is not typed: %s", raw)
	}
	if !strings.Contains(def.Description, "Unit: token") {
		t.Fatalf("definition lacks the unit: %s", def.Description)
	}

	call := func(args map[string]any) (delegateResult, mcp.RunToolOutput, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "agent", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		structured, _ := json.Marshal(res.StructuredContent)
		var out mcp.RunToolOutput
		_ = json.Unmarshal(structured, &out)
		var env delegateResult
		if err := json.Unmarshal([]byte(out.Stdout), &env); err != nil {
			t.Fatalf("not an envelope: %s (%v)", structured, err)
		}
		return env, out, res.IsError
	}
	env, out, failed := call(map[string]any{"agent": "codex", "instruction": "review this"})
	if failed || env.ExitCode != 0 || !strings.Contains(env.Output, "reviewed") {
		t.Fatalf("call failed: %+v\n%s", env, out.Stderr)
	}
	if env.Usage == nil || env.Usage.Input != 21 || env.Usage.Output != 9 {
		t.Fatalf("usage: %+v", env.Usage)
	}
	if exeName(fake.agent) != "codex" || fake.args[len(fake.args)-1] != "review this" {
		t.Fatalf("fake tool saw %q %v", fake.agent, fake.args)
	}
	if strings.Contains(strings.Join(fake.args, " "), "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("the agent tool must launch read-only: %v", fake.args)
	}

	// A tool that reports no usage yields none.
	fake.output = "plain prose\n"
	if env, _, _ := call(map[string]any{"agent": "codex", "instruction": "again"}); env.Usage != nil {
		t.Fatalf("usage guessed: %+v", env.Usage)
	}
	// A required token cap the adapter cannot enforce is a typed refusal.
	fake.agent = ""
	env, _, failed = call(map[string]any{"agent": "codex", "instruction": "capped", "max-tokens": 1000})
	if !failed || env.Error == nil || env.Error.Kind != "budget" || fake.agent != "" {
		t.Fatalf("cap: %+v launched=%q", env, fake.agent)
	}
}
