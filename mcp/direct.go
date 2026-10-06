package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"weak"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

// Each server owns its synthetic effects and refresh state. Weak keys and a
// cleanup prevent the index from retaining servers after callers release them.
var directStates sync.Map // map[weak.Pointer[mcpsdk.Server]]*directState

type directState struct {
	mu       sync.Mutex
	policy   *Policy
	names    []string
	reserved map[string]bool
}

func directStateFor(srv *mcpsdk.Server, policy *Policy) *directState {
	key := weak.Make(srv)
	if value, ok := directStates.Load(key); ok {
		return value.(*directState)
	}
	p := &Policy{}
	if policy != nil {
		*p = *policy
	}
	p.synthetic = &sync.Map{}
	state := &directState{policy: p, reserved: map[string]bool{"list_tools": true, "run_tool": true, "server_info": true, "bashy": true}}
	value, loaded := directStates.LoadOrStore(key, state)
	if loaded {
		return value.(*directState)
	}
	runtime.AddCleanup(srv, func(key weak.Pointer[mcpsdk.Server]) { directStates.Delete(key) }, key)
	srv.AddReceivingMiddleware(p.middleware)
	return state
}

// RegisteredCommand adapts a caller-owned command to a direct MCP tool.
// Schema parameters named stdin or dir take precedence over transport inputs.
type RegisteredCommand struct {
	Name, Synopsis, Usage string
	Effects, OS           []string
	Schema                *tool.ArgSchema
	Run                   func(ctx context.Context, argv []string, stdin, dir string) (stdout, stderr string, exit int)
}

var directToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// directInput keeps command identity server-owned while accepting the same
// execution inputs as run_tool.
type directInput struct {
	Args  []string          `json:"args,omitempty"`
	Stdin string            `json:"stdin,omitempty"`
	Dir   string            `json:"dir,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
}

// RegisterDirectTools exposes the explicitly selected registry commands.
// AllTools adds canonical commands supported on this server's operating system.
func RegisterDirectTools(srv *mcpsdk.Server, opts Options) error {
	names := append([]string(nil), opts.Tools...)
	if opts.AllTools {
		for _, name := range tool.Names() {
			if entry, ok := atlas.Lookup(name); ok && (entry.AliasOf != "" || !slices.Contains(entry.OS, runtime.GOOS)) {
				continue
			}
			names = append(names, name)
		}
	}
	state := directStateFor(srv, opts.Policy)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := registerRegistryTools(srv, state.policy, names); err != nil {
		return err
	}
	for _, name := range names {
		if directToolName.MatchString(name) {
			state.reserved[name] = true
		}
	}
	if err := refreshRegistered(srv, state, opts); err != nil {
		return err
	}
	if opts.RunScript != nil {
		registerScript(srv, state, opts)
	}
	return nil
}

// registerRegistryTools is the generic registry-command part of direct tool
// registration. Profile selection and registered-command adapters must supply
// the selected names; this helper deliberately does not choose an exposure set.
func registerRegistryTools(srv *mcpsdk.Server, policy *Policy, names []string) error {
	names = append([]string(nil), names...)
	sort.Strings(names)
	descriptions := make([]*mcpsdk.Tool, 0, len(names))
	previous := ""
	for _, name := range names {
		if !directToolName.MatchString(name) {
			continue
		}
		if name == previous {
			continue
		}
		previous = name
		command := tool.Lookup(name)
		if command == nil {
			return fmt.Errorf("unknown command: %s", name)
		}
		usage, _, _ := strings.Cut(command.Usage, "\n")
		doc := tool.DocumentAsMCP(name, command.Synopsis+" — "+usage)
		if entry, ok := atlas.Lookup(name); ok {
			doc.Tool.ApplyEffects(entry.Effects, entry.OS)
		}
		properties := doc.Tool.InputSchema["properties"].(map[string]any)
		properties["stdin"] = map[string]any{"type": "string"}
		properties["dir"] = map[string]any{"type": "string"}
		properties["env"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
		description, err := sdkTool(doc.Tool)
		if err != nil {
			return err
		}
		descriptions = append(descriptions, description)
	}
	// Validate the whole selection before mutating the server.
	for _, description := range descriptions {
		name := description.Name
		mcpsdk.AddTool(srv, description, func(ctx context.Context, req *mcpsdk.CallToolRequest, in directInput) (*mcpsdk.CallToolResult, RunToolOutput, error) {
			return policy.runToolHandler(ctx, req, RunToolInput{Name: name, Args: in.Args, Stdin: in.Stdin, Dir: in.Dir, Env: in.Env})
		})
	}
	return nil
}

// sdkTool preserves the descriptor's JSON schema and metadata. SDK v1.8.0
// uses bare bools for readOnlyHint/idempotentHint and pointers for the other
// hints; unmarshalling maps absent bools to their protocol default (false).
func sdkTool(doc tool.MCPToolDescription) (*mcpsdk.Tool, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", doc.Name, err)
	}
	var description mcpsdk.Tool
	if err := json.Unmarshal(data, &description); err != nil {
		return nil, fmt.Errorf("describe %s: %w", doc.Name, err)
	}
	return &description, nil
}

// NotifyToolsChanged refreshes the registered-command snapshot. SDK additions
// and removals emit notifications/tools/list_changed to subscribed clients.
// Options.Policy does not replace the policy established at server creation.
func NotifyToolsChanged(srv *mcpsdk.Server, opts Options) error {
	state := directStateFor(srv, opts.Policy)
	state.mu.Lock()
	defer state.mu.Unlock()
	return refreshRegistered(srv, state, opts)
}

func refreshRegistered(srv *mcpsdk.Server, state *directState, opts Options) error {
	var commands []RegisteredCommand
	if opts.Registered != nil {
		commands = append(commands, opts.Registered()...)
	}
	slices.SortFunc(commands, func(a, b RegisteredCommand) int { return strings.Compare(a.Name, b.Name) })
	type prepared struct {
		command     RegisteredCommand
		description *mcpsdk.Tool
	}
	var ready []prepared
	seen := map[string]bool{}
	for _, command := range commands {
		if !directToolName.MatchString(command.Name) {
			continue
		}
		if state.reserved[command.Name] || seen[command.Name] {
			return fmt.Errorf("duplicate tool name: %s", command.Name)
		}
		seen[command.Name] = true
		if command.Run == nil {
			return fmt.Errorf("registered command %s has no runner", command.Name)
		}
		usage, _, _ := strings.Cut(command.Usage, "\n")
		doc := tool.DocumentAsMCP(command.Name, command.Synopsis+" — "+usage)
		if command.Schema != nil {
			doc = tool.DocumentAsMCPSchema(command.Name, command.Synopsis+" — "+usage, *command.Schema)
		}
		props := doc.Tool.InputSchema["properties"].(map[string]any)
		for _, key := range []string{"stdin", "dir"} {
			if _, exists := props[key]; !exists {
				props[key] = map[string]any{"type": "string"}
			}
		}
		command.Effects = append([]string(nil), command.Effects...)
		doc.Tool.ApplyEffects(command.Effects, command.OS)
		description, err := sdkTool(doc.Tool)
		if err != nil {
			return err
		}
		// Resolve before mutation so invalid defaults return an error instead
		// of letting the SDK panic midway through replacing the snapshot.
		data, err := json.Marshal(description.InputSchema)
		if err != nil {
			return err
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			return fmt.Errorf("schema %s: %w", command.Name, err)
		}
		if _, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true}); err != nil {
			return fmt.Errorf("schema %s: %w", command.Name, err)
		}
		ready = append(ready, prepared{command, description})
	}
	// Validate the full snapshot before removing any previously registered tools.
	srv.RemoveTools(state.names...)
	for _, name := range state.names {
		state.policy.synthetic.Delete(name)
	}
	state.names = nil
	for _, item := range ready {
		command := item.command
		state.policy.synthetic.Store(command.Name, command.Effects)
		mcpsdk.AddTool(srv, item.description, func(ctx context.Context, req *mcpsdk.CallToolRequest, _ map[string]any) (*mcpsdk.CallToolResult, RunToolOutput, error) {
			// Recheck the captured effects: a refresh can replace the lookup while
			// an invocation of this older handler is already in flight.
			if err := state.policy.Check(command.Name, command.Effects); err != nil {
				res, out := state.policy.policyDenial(command.Name, err)
				return res, out, nil
			}
			// UseNumber preserves large integers for ArgSchema.Argv.
			arguments := map[string]any{}
			raw := req.Params.Arguments
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&arguments); err != nil {
				return nil, RunToolOutput{}, err
			}
			var argv []string
			var stdin, dir string
			if command.Schema != nil {
				props := command.Schema.JSONSchema()["properties"].(map[string]any)
				if _, exists := props["stdin"]; !exists {
					stdin, _ = arguments["stdin"].(string)
					delete(arguments, "stdin")
				}
				if _, exists := props["dir"]; !exists {
					dir, _ = arguments["dir"].(string)
					delete(arguments, "dir")
				}
				var err error
				argv, err = command.Schema.Argv(arguments)
				if err != nil {
					return nil, RunToolOutput{}, err
				}
			} else {
				var in directInput
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, RunToolOutput{}, err
				}
				argv, stdin, dir = in.Args, in.Stdin, in.Dir
			}
			stdout, stderr, exit := command.Run(ctx, argv, stdin, dir)
			return &mcpsdk.CallToolResult{IsError: exit != 0}, RunToolOutput{Stdout: stdout, Stderr: stderr, ExitCode: exit}, nil
		})
		state.names = append(state.names, command.Name)
	}
	return nil
}

type scriptInput struct {
	Script string `json:"script"`
	Stdin  string `json:"stdin,omitempty"`
	Dir    string `json:"dir,omitempty"`
}

func registerScript(srv *mcpsdk.Server, state *directState, opts Options) {
	doc := tool.MCPToolDescription{
		Name: "bashy", Description: "Run literal script bytes through the bashy interpreter.",
		InputSchema: map[string]any{
			"type": "object", "required": []string{"script"}, "additionalProperties": false,
			"properties": map[string]any{"script": map[string]any{"type": "string"}, "stdin": map[string]any{"type": "string"}, "dir": map[string]any{"type": "string"}},
		},
	}
	doc.ApplyEffects([]string{"exec"}, []string{runtime.GOOS})
	description, _ := sdkTool(doc) // fixed, JSON-compatible descriptor
	state.policy.synthetic.Store("bashy", []string{"exec"})
	mcpsdk.AddTool(srv, description, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in scriptInput) (*mcpsdk.CallToolResult, RunToolOutput, error) {
		stdout, stderr, exit, err := opts.RunScript(ctx, in.Script, in.Stdin, in.Dir)
		result := &mcpsdk.CallToolResult{IsError: exit != 0}
		if err != nil {
			result.SetError(err)
		}
		return result, RunToolOutput{Stdout: stdout, Stderr: stderr, ExitCode: exit}, nil
	})
}

// DeclareSyntheticEffects records the atlas effects of a tool the caller
// registers on srv itself (one that is neither a registry command nor a
// registered command), so the policy gate recognizes the name and enforces
// its effects. Call it after BuildServer and before serving.
func DeclareSyntheticEffects(srv *mcpsdk.Server, name string, effects []string) {
	state := directStateFor(srv, nil)
	state.policy.synthetic.Store(name, append([]string(nil), effects...))
}
