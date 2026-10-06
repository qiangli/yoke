package mcp

import (
	"encoding/json"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
	"runtime"
	"slices"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDirectRegistryCalls(t *testing.T) {
	for _, tc := range []struct {
		name           string
		arguments      map[string]any
		stdout, stderr string
		exit           int
		denied         bool
	}{
		{name: "echo", arguments: map[string]any{"args": []string{"hi"}}, stdout: "hi\n"},
		{name: "mcpprobe", arguments: map[string]any{"args": []string{"x", "y"}, "stdin": "hello\n"}, stdout: "out:hello:x,y"},
		{name: "mcpprobe", arguments: map[string]any{"args": []string{"fail"}, "stdin": "z"}, stdout: "out:z:fail", stderr: "boom", exit: 5},
		{name: "rm", arguments: map[string]any{"args": []string{"unused"}}, stderr: "denied: effect destroy requires --allow destroy", exit: 126, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &Policy{Audit: func(Record) {}}
			srv := NewServerWithOptions("direct", "test", Options{Policy: policy})
			if err := registerRegistryTools(srv, policy, []string{tc.name}); err != nil {
				t.Fatal(err)
			}
			cs, ctx := policyClient(t, srv)
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: tc.name, Arguments: tc.arguments})
			if err != nil {
				t.Fatal(err)
			}
			out := decodeStructured[RunToolOutput](t, result)
			if out.Stdout != tc.stdout || out.Stderr != tc.stderr || out.ExitCode != tc.exit {
				t.Fatalf("got %#v", out)
			}
			if tc.denied && (!result.IsError || len(result.Content) != 1 || result.Content[0].(*mcpsdk.TextContent).Text != tc.stderr) {
				t.Fatalf("denial: %#v", result)
			}
		})
	}
}

func TestDirectRegistryDeterminism(t *testing.T) {
	var previous []byte
	for _, names := range [][]string{{"rm", "echo", "mcpprobe", "echo"}, {"mcpprobe", "echo", "rm"}} {
		srv := NewServerWithOptions("direct", "test", Options{Tools: names})
		cs, ctx := policyClient(t, srv)
		result, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil && string(previous) != string(data) {
			t.Fatalf("tools/list differs:\n%s\n%s", previous, data)
		}
		previous = data
	}
}

func TestDirectRegistrySelectionValidation(t *testing.T) {
	srv := NewServer("direct", "test")
	if err := registerRegistryTools(srv, nil, []string{"echo", "zz-missing-command"}); err == nil || err.Error() != "unknown command: zz-missing-command" {
		t.Fatalf("unknown: %v", err)
	}
	if err := registerRegistryTools(srv, nil, []string{"[", "bad/name", strings.Repeat("a", 65)}); err != nil {
		t.Fatal(err)
	}
	cs, ctx := policyClient(t, srv)
	result, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if tool.Name == "echo" {
			t.Fatal("failed selection partially registered tools")
		}
	}
}

func TestDirectRegistrySchema(t *testing.T) {
	srv := NewServer("direct", "test")
	if err := registerRegistryTools(srv, nil, []string{"echo"}); err != nil {
		t.Fatal(err)
	}
	cs, ctx := policyClient(t, srv)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name != "echo" {
			continue
		}
		found = true
		schema := tool.InputSchema.(map[string]any)
		properties := schema["properties"].(map[string]any)
		for _, key := range []string{"args", "stdin", "dir", "env"} {
			if properties[key] == nil {
				t.Errorf("missing property %s", key)
			}
		}
		if properties["name"] != nil {
			t.Fatal("client can supply command identity")
		}
	}
	if !found {
		t.Fatal("echo not listed")
	}
	for _, arguments := range []map[string]any{{"args": "hi"}, {"name": "rm"}, {"env": map[string]any{"BAD": true}}} {
		result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "echo", Arguments: arguments})
		if err == nil && !result.IsError {
			t.Fatalf("accepted invalid arguments: %#v", arguments)
		}
	}
}

func TestDirectEffectOverride(t *testing.T) {
	commandEffectOverrides.Store("mcp-synthetic-probe", []string{"exec"})
	t.Cleanup(func() { commandEffectOverrides.Delete("mcp-synthetic-probe") })
	effects := commandEffects("mcp-synthetic-probe")
	if len(effects) != 1 || effects[0] != "exec" {
		t.Fatalf("effects: %v", effects)
	}
	effects[0] = "pure"
	if commandEffects("mcp-synthetic-probe")[0] != "exec" {
		t.Fatal("caller mutated stored effects")
	}
}

func TestDirectOptionsSelection(t *testing.T) {
	for _, all := range []bool{false, true} {
		srv := NewServerWithOptions("selection", "test", Options{AllTools: all})
		cs, ctx := policyClient(t, srv)
		listed, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, d := range listed.Tools {
			got[d.Name] = true
		}
		if !all && len(got) != 3 {
			t.Fatalf("default tools: %v", got)
		}
		if all {
			for _, name := range tool.Names() {
				want := directToolName.MatchString(name)
				if e, ok := atlas.Lookup(name); ok {
					want = want && e.AliasOf == "" && slices.Contains(e.OS, runtime.GOOS)
				}
				if got[name] != want {
					t.Errorf("%s: present=%v want=%v", name, got[name], want)
				}
			}
		}
	}
}

func TestDirectAtlasMetadata(t *testing.T) {
	srv := NewServerWithOptions("metadata", "test", Options{Tools: []string{"echo", "rm"}})
	cs, ctx := policyClient(t, srv)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed.Tools {
		if d.Name != "echo" && d.Name != "rm" {
			continue
		}
		e, _ := atlas.Lookup(d.Name)
		expected, _ := json.Marshal(map[string]any{"bashy.effects": e.Effects, "bashy.os": e.OS})
		actual, _ := json.Marshal(d.Meta)
		if string(actual) != string(expected) {
			t.Fatalf("%s metadata: %s != %s", d.Name, actual, expected)
		}
		if d.Annotations == nil || d.Annotations.DestructiveHint == nil || *d.Annotations.DestructiveHint != (d.Name == "rm") {
			t.Fatalf("%s annotations: %+v", d.Name, d.Annotations)
		}
	}
}
