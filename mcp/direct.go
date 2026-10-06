package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
)

// commandEffectOverrides carries declared effects for synthetic tool names.
// Writers store immutable slices; readers receive a copy from commandEffects.
var commandEffectOverrides sync.Map // map[string][]string

var directToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// directInput keeps command identity server-owned while accepting the same
// execution inputs as run_tool.
type directInput struct {
	Args  []string          `json:"args,omitempty"`
	Stdin string            `json:"stdin,omitempty"`
	Dir   string            `json:"dir,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
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
		properties := doc.Tool.InputSchema["properties"].(map[string]any)
		properties["stdin"] = map[string]any{"type": "string"}
		properties["dir"] = map[string]any{"type": "string"}
		properties["env"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
		// The SDK's InputSchema is any, Annotations is *ToolAnnotations, and Meta
		// is an embedded map. JSON conversion preserves the descriptor's wire
		// representation without assuming these are identical Go field types.
		data, err := json.Marshal(doc.Tool)
		if err != nil {
			return fmt.Errorf("describe %s: %w", name, err)
		}
		var description mcpsdk.Tool
		if err := json.Unmarshal(data, &description); err != nil {
			return fmt.Errorf("describe %s: %w", name, err)
		}
		descriptions = append(descriptions, &description)
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
