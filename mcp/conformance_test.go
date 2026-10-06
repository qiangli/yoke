package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/qiangli/coreutils/cmds/all"
)

func TestProtocolConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := NewServer("bashy", "test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "conformance", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	t.Run("protocol", func(t *testing.T) {
		// go-sdk v1.8.0 exposes the negotiated discovery/initialize result here.
		result := session.InitializeResult()
		if result == nil {
			t.Fatal("missing initialize result")
		}
		t.Logf("negotiated protocol version: %s", result.ProtocolVersion)
		if result.ProtocolVersion != "2026-07-28" {
			t.Errorf("protocol version = %q, want 2026-07-28", result.ProtocolVersion)
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		result := session.InitializeResult()
		if result == nil || result.Capabilities == nil {
			t.Fatal("missing server capabilities")
		}
		// Roots and sampling are client capabilities in the SDK. Check the
		// serialized server advertisement for all three deprecated keys.
		data, err := json.Marshal(result.Capabilities)
		if err != nil {
			t.Fatal(err)
		}
		var capabilities map[string]json.RawMessage
		if err := json.Unmarshal(data, &capabilities); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"roots", "sampling", "logging"} {
			if _, exists := capabilities[name]; exists {
				t.Errorf("server advertises %s: %s", name, data)
			}
		}
	})

	t.Run("tools_list", func(t *testing.T) {
		first, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range first.Tools {
			names = append(names, tool.Name)
		}
		if !slices.Equal(names, []string{"list_tools", "run_tool"}) {
			t.Errorf("tools/list = %v, want [list_tools run_tool]", names)
		}
		if first.NextCursor != "" {
			t.Errorf("unexpected next page: %q", first.NextCursor)
		}
		second, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		firstJSON, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		secondJSON, err := json.Marshal(second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstJSON, secondJSON) {
			t.Errorf("tools/list JSON changed:\n%s\n%s", firstJSON, secondJSON)
		}
	})

	t.Run("run_echo", func(t *testing.T) {
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "run_tool",
			Arguments: map[string]any{"name": "echo", "args": []string{"hi"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("run_tool returned an MCP error: %+v", result)
		}
		out := decodeStructured[RunToolOutput](t, result)
		if out.Stdout != "hi\n" || out.ExitCode != 0 {
			t.Errorf("run_tool = %+v, want stdout hi\\n and exit_code 0", out)
		}
	})
}
