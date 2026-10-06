package mcp

import (
	"context"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHTTPPrebuiltToolsAndRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := Options{Policy: &Policy{Audit: func(Record) {}}}
	srv, err := BuildServer("test", "1", opts)
	if err != nil {
		t.Fatal(err)
	}
	DeclareSyntheticEffects(srv, "shell_probe", []string{"exec"})
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "shell_probe"}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, RunToolOutput, error) {
		return nil, RunToolOutput{Stdout: "attached"}, nil
	})
	addr, shutdown, err := ServeHTTPServerWithShutdown(ctx, srv, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "shell_probe", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("attached tool: %v %+v", err, result)
	}
	opts.Registered = func() []RegisteredCommand {
		return []RegisteredCommand{{Name: "refreshed", Effects: []string{"pure"}, Run: func(context.Context, []string, string, string) (string, string, int) { return "", "", 0 }}}
	}
	if err := NotifyToolsChanged(srv, opts); err != nil {
		t.Fatal(err)
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Tools {
		if item.Name == "refreshed" {
			return
		}
	}
	t.Fatal("HTTP did not expose refreshed tool")
}
