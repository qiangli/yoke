package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
	"io"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	p := &Policy{synthetic: &sync.Map{}}
	p.synthetic.Store("mcp-synthetic-probe", []string{"exec"})
	effects := p.commandEffects("mcp-synthetic-probe")
	if len(effects) != 1 || effects[0] != "exec" {
		t.Fatalf("effects: %v", effects)
	}
	effects[0] = "pure"
	if p.commandEffects("mcp-synthetic-probe")[0] != "exec" {
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

func TestRegisteredSchemaAndPolicy(t *testing.T) {
	schema := &tool.ArgSchema{
		Positionals: []tool.ArgParameter{{Name: "source", Required: true}, {Name: "target", Required: true}},
		Flags:       []tool.ArgFlag{{Name: "count", Type: "int", Default: "2"}},
	}
	calls := 0
	command := RegisteredCommand{Name: "registered-copy", Synopsis: "Copy", Usage: "copy SOURCE TARGET\nmore", Schema: schema, Effects: []string{"destroy"}, OS: []string{"darwin", "linux"},
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			calls++
			if ctx == nil || !reflect.DeepEqual(argv, []string{"--count", "2", "a", "b"}) || stdin != "input" || dir != "working" {
				t.Errorf("invocation: %v %q %q", argv, stdin, dir)
			}
			return "output", "error", 7
		},
	}
	for _, allow := range []bool{false, true} {
		opts := Options{Policy: &Policy{Allow: map[string]bool{"destroy": allow}, Audit: func(Record) {}}, Registered: func() []RegisteredCommand { return []RegisteredCommand{command} }}
		cs, ctx := policyClient(t, NewServerWithOptions("registered", "test", opts))
		listed, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range listed.Tools {
			if d.Name != command.Name {
				continue
			}
			found = true
			props := d.InputSchema.(map[string]any)["properties"].(map[string]any)
			if props["count"].(map[string]any)["type"] != "integer" || props["args"] != nil || !*d.Annotations.DestructiveHint {
				t.Fatalf("descriptor: %+v", d)
			}
		}
		if !found {
			t.Fatal("missing registered tool")
		}
		result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: command.Name, Arguments: map[string]any{"source": "a", "target": "b", "stdin": "input", "dir": "working"}})
		if err != nil {
			t.Fatal(err)
		}
		out := decodeStructured[RunToolOutput](t, result)
		if !result.IsError {
			t.Fatal("expected tool error")
		}
		if allow {
			if out != (RunToolOutput{Stdout: "output", Stderr: "error", ExitCode: 7}) {
				t.Fatalf("output: %+v", out)
			}
		} else if out.ExitCode != 126 {
			t.Fatalf("denial: %+v", out)
		}
	}
	if calls != 1 {
		t.Fatalf("runner called %d times", calls)
	}
}

func TestRegisteredRefreshNotification(t *testing.T) {
	command := RegisteredCommand{Name: "registered-first", Effects: []string{"pure"}, Run: func(_ context.Context, args []string, stdin, dir string) (string, string, int) {
		return strings.Join(args, ",") + stdin + dir, "", 0
	}}
	commands := []RegisteredCommand{command}
	opts := Options{Policy: &Policy{Audit: func(Record) {}}, Registered: func() []RegisteredCommand { return commands }}
	srv := NewServerWithOptions("refresh", "test", opts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	changed := make(chan struct{}, 10)
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "refresh-client", Version: "1"}, &mcpsdk.ClientOptions{ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
		select {
		case changed <- struct{}{}:
		default:
		}
	}})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	command.Name = "registered-added"
	commands = append(commands, command)
	if err := NotifyToolsChanged(srv, opts); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("no tools/list_changed notification")
	}
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, d := range listed.Tools {
		names = append(names, d.Name)
	}
	if !slices.Contains(names, command.Name) {
		t.Fatalf("new tool absent: %v", names)
	}
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: command.Name, Arguments: map[string]any{"args": []string{"x", "y"}, "stdin": "z", "dir": "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeStructured[RunToolOutput](t, result); out.Stdout != "x,yzd" {
		t.Fatalf("output: %+v", out)
	}
	commands = nil
	if err := NotifyToolsChanged(srv, opts); err != nil {
		t.Fatal(err)
	}
	listed, err = cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 3 {
		t.Fatalf("removed tools remain: %+v", listed.Tools)
	}
}

func TestRegisteredEffectsAreServerLocal(t *testing.T) {
	command := RegisteredCommand{Name: "registered-local", Effects: []string{"destroy"}, Run: func(context.Context, []string, string, string) (string, string, int) { return "ran", "", 0 }}
	opts := Options{Policy: &Policy{Audit: func(Record) {}}, Registered: func() []RegisteredCommand { return []RegisteredCommand{command} }}
	blocked := NewServerWithOptions("blocked", "test", opts)
	command.Effects = []string{"pure"}
	allowed := NewServerWithOptions("allowed", "test", opts)
	for _, tc := range []struct {
		srv  *mcpsdk.Server
		exit int
	}{{blocked, 126}, {allowed, 0}} {
		cs, ctx := policyClient(t, tc.srv)
		result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: command.Name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if out := decodeStructured[RunToolOutput](t, result); out.ExitCode != tc.exit {
			t.Fatalf("output: %+v", out)
		}
	}
}

func TestRegisteredCannotWeakenRegistryPolicy(t *testing.T) {
	opts := Options{Policy: &Policy{Audit: func(Record) {}}, Registered: func() []RegisteredCommand {
		return []RegisteredCommand{{Name: "rm", Effects: []string{"pure"}, Run: func(context.Context, []string, string, string) (string, string, int) { return "", "", 0 }}}
	}}
	cs, ctx := policyClient(t, NewServerWithOptions("collision", "test", opts))
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: RunToolInput{Name: "rm", Args: []string{"unused"}}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeStructured[RunToolOutput](t, result); out.ExitCode != 126 {
		t.Fatalf("registry policy weakened: %+v", out)
	}
}

func TestRegisteredRefreshValidation(t *testing.T) {
	command := RegisteredCommand{Name: "registered-stable", Run: func(context.Context, []string, string, string) (string, string, int) { return "", "", 0 }}
	commands := []RegisteredCommand{command}
	opts := Options{Registered: func() []RegisteredCommand { return commands }}
	srv := NewServerWithOptions("validation", "test", opts)
	for _, invalid := range []RegisteredCommand{{Name: "run_tool", Run: command.Run}, {Name: "no-runner"}, command} {
		commands = []RegisteredCommand{command, invalid}
		if err := NotifyToolsChanged(srv, opts); err == nil {
			t.Fatalf("accepted invalid command: %+v", invalid)
		}
		cs, ctx := policyClient(t, srv)
		listed, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Tools) != 4 {
			t.Fatalf("failed update changed tools: %+v", listed.Tools)
		}
	}
}

func TestScriptTool(t *testing.T) {
	const script = "echo '$HOME'; printf \"%s\" \"$(literal)\"\n"
	for _, tc := range []struct {
		name string
		exit int
		err  error
	}{{"success", 0, nil}, {"exit", 9, nil}, {"error", 0, errors.New("interpreter failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			records := make(chan Record, 10)
			opts := Options{Policy: &Policy{Audit: func(r Record) { records <- r }}, RunScript: func(ctx context.Context, got, stdin, dir string) (string, string, int, error) {
				calls++
				if ctx == nil || got != script || stdin != "input" || dir != "working" {
					t.Errorf("script invocation: %q %q %q", got, stdin, dir)
				}
				return "output", "diagnostic", tc.exit, tc.err
			}}
			cs, ctx := policyClient(t, NewServerWithOptions("script", "test", opts))
			listed, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range listed.Tools {
				if d.Name != "bashy" {
					continue
				}
				found = true
				if d.Annotations == nil || d.Annotations.ReadOnlyHint || !reflect.DeepEqual(d.Meta["bashy.effects"], []any{"exec"}) {
					t.Fatalf("script descriptor: %+v", d)
				}
			}
			if !found {
				t.Fatal("bashy not registered")
			}
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "bashy", Arguments: map[string]any{"script": script, "stdin": "input", "dir": "working"}})
			if err != nil {
				t.Fatal(err)
			}
			if out := decodeStructured[RunToolOutput](t, result); out != (RunToolOutput{Stdout: "output", Stderr: "diagnostic", ExitCode: tc.exit}) {
				t.Fatalf("output: %+v", out)
			}
			if result.IsError != (tc.exit != 0 || tc.err != nil) {
				t.Fatalf("result: %+v", result)
			}
			record := <-records
			if !record.Allowed || !reflect.DeepEqual(record.Effects, []string{"exec"}) {
				t.Fatalf("audit: %+v", record)
			}
			for _, invalid := range []map[string]any{{}, {"script": 42}, {"script": "true", "args": []string{}}} {
				result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "bashy", Arguments: invalid})
				if err == nil && !result.IsError {
					t.Fatalf("accepted invalid input: %v", invalid)
				}
			}
			if calls != 1 {
				t.Fatalf("runner called %d times", calls)
			}
		})
	}
	cs, ctx := policyClient(t, NewServerWithOptions("no-script", "test", Options{}))
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed.Tools {
		if d.Name == "bashy" {
			t.Fatal("bashy registered without RunScript")
		}
	}
}

func init() {
	// Include real atlas entries for platform and alias selection without
	// executing platform-specific commands in this package's test suite.
	for _, name := range []string{"ps", "docker"} {
		if tool.Lookup(name) == nil {
			tool.Register(&tool.Tool{Name: name, Run: func(*tool.RunContext, []string) int { return 0 }})
		}
	}
	tool.Register(&tool.Tool{Name: "mcp-context-probe", Run: func(rc *tool.RunContext, args []string) int {
		input, _ := io.ReadAll(rc.In)
		environment := append([]string(nil), rc.Env...)
		slices.Sort(environment)
		data, _ := json.Marshal([]any{args, string(input), rc.Dir, environment, rc.Ctx != nil, rc.FS != nil})
		rc.Out.Write(data)
		return 0
	}})
}

func TestDirectRunContextMatchesCompat(t *testing.T) {
	cs, ctx := policyClient(t, NewServerWithOptions("context", "test", Options{Tools: []string{"mcp-context-probe"}, Policy: &Policy{Audit: func(Record) {}}}))
	arguments := map[string]any{"args": []string{"one", "two"}, "stdin": "bytes\n", "dir": "working", "env": map[string]string{"Z": "last", "A": "first"}}
	direct, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "mcp-context-probe", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	arguments["name"] = "mcp-context-probe"
	compat, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_tool", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeStructured[RunToolOutput](t, direct)
	if want := decodeStructured[RunToolOutput](t, compat); got != want {
		t.Fatalf("direct %+v != compat %+v", got, want)
	}
	if got.Stdout != `[["one","two"],"bytes\n","working",["A=first","Z=last"],true,true]` {
		t.Fatalf("context: %s", got.Stdout)
	}
}

func TestRegisteredArgumentValidation(t *testing.T) {
	calls := 0
	command := RegisteredCommand{Name: "registered-typed", Schema: &tool.ArgSchema{
		Positionals: []tool.ArgParameter{{Name: "number", Type: "int", Required: true}},
		Flags:       []tool.ArgFlag{{Name: "mode", Enum: []string{"fast", "slow"}, Default: "fast"}, {Name: "verbose", Type: "bool"}},
	}, Run: func(_ context.Context, args []string, _, _ string) (string, string, int) {
		calls++
		return strings.Join(args, "|"), "", 0
	}}
	cs, ctx := policyClient(t, NewServerWithOptions("typed", "test", Options{Policy: &Policy{Audit: func(Record) {}}, Registered: func() []RegisteredCommand { return []RegisteredCommand{command} }}))
	for _, arguments := range []map[string]any{{}, {"number": "1"}, {"number": 1.5}, {"number": 1, "mode": "invalid"}, {"number": 1, "extra": true}} {
		result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: command.Name, Arguments: arguments})
		if err == nil && !result.IsError {
			t.Fatalf("accepted invalid arguments: %v", arguments)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid arguments reached runner %d times", calls)
	}
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: command.Name, Arguments: map[string]any{"number": json.Number("9007199254740993"), "verbose": true}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeStructured[RunToolOutput](t, result); out.Stdout != "--mode|fast|--verbose|9007199254740993" {
		t.Fatalf("argv: %+v", out)
	}
}

func TestDescriptorConversion(t *testing.T) {
	yes, no := true, false
	doc := tool.DocumentAsMCP("fixture", "description").Tool
	doc.Annotations = &tool.MCPToolAnnotations{Title: "Fixture", ReadOnlyHint: &no, IdempotentHint: &yes, DestructiveHint: &no, OpenWorldHint: &yes}
	doc.Meta = map[string]any{"custom": map[string]any{"value": "preserved"}}
	converted, err := sdkTool(doc)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(doc)
	actual, _ := json.Marshal(converted)
	if string(original) == string(actual) {
		return
	}
	var a, b any
	json.Unmarshal(original, &a)
	json.Unmarshal(actual, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("descriptor changed: %s != %s", actual, original)
	}
}

func TestRegisteredInvalidSchemaPreservesTools(t *testing.T) {
	command := RegisteredCommand{Name: "registered-schema", Run: func(context.Context, []string, string, string) (string, string, int) { return "", "", 0 }}
	opts := Options{Registered: func() []RegisteredCommand { return []RegisteredCommand{command} }}
	srv := NewServerWithOptions("schema", "test", opts)
	command.Schema = &tool.ArgSchema{Flags: []tool.ArgFlag{{Name: "count", Type: "int", Default: "2", Enum: []string{"1"}}}}
	if err := NotifyToolsChanged(srv, opts); err == nil {
		t.Fatal("accepted invalid schema default")
	}
	cs, ctx := policyClient(t, srv)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed.Tools {
		if d.Name == command.Name && d.InputSchema.(map[string]any)["properties"].(map[string]any)["args"] != nil {
			return
		}
	}
	t.Fatal("invalid refresh replaced the previous tool")
}

func TestBuildServerUnknownToolNameErrors(t *testing.T) {
	if _, err := BuildServer("t", "0", Options{Tools: []string{"no-such-command-xyz"}}); err == nil {
		t.Fatal("BuildServer accepted an unknown tool name")
	}
}

func TestDeclareSyntheticEffects(t *testing.T) {
	srv, err := BuildServer("t", "0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	DeclareSyntheticEffects(srv, "shell_exec", []string{"exec"})
	state := directStateFor(srv, nil)
	if !state.policy.knownCommand("shell_exec") {
		t.Fatal("synthetic name not known to the policy")
	}
	if got := state.policy.commandEffects("shell_exec"); len(got) != 1 || got[0] != "exec" {
		t.Fatalf("effects = %v, want [exec]", got)
	}
}
