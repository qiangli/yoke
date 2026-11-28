// Package offer describes the typed calls a fleet can share. It sits above
// fleet because the broker and chat one-shot adapters both consume fleet.
package offer

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

type ToolDefinition struct {
	Name        string             `json:"name"`
	Class       string             `json:"class"`
	Description string             `json:"description"`
	Input       map[string]any     `json:"input"`
	Output      *jsonschema.Schema `json:"output"`
	Effects     []string           `json:"effects"`
	Unit        string             `json:"unit"`
	Tags        []string           `json:"tags,omitempty"`
}

func (d ToolDefinition) WithTags(words ...string) ToolDefinition {
	tags := slices.Clone(d.Tags)
	for _, word := range words {
		for _, tag := range strings.Split(word, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	slices.Sort(tags)
	d.Tags = slices.Compact(tags)
	return d
}
func (d ToolDefinition) HasTag(tag string) bool  { return slices.Contains(d.Tags, tag) }
func (d ToolDefinition) Digest() (string, error) { return mcp.CanonicalSHA256(d.WithTags()) }

// FilterByTag preserves listing order. An empty filter lists all offers.
func FilterByTag(defs []ToolDefinition, tag string) []ToolDefinition {
	return slices.DeleteFunc(slices.Clone(defs), func(d ToolDefinition) bool { return tag != "" && !d.HasTag(tag) })
}

// TagsInUse is the vocabulary of this listing, including fleet capabilities.
func TagsInUse(defs []ToolDefinition) []string {
	var tags []string
	for _, d := range defs {
		tags = append(tags, d.Tags...)
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}

// Offerable resolves a name without publishing it. A qualified tool:command
// identifies one vendor feature even when several tools implement it.
func Offerable(c *fleet.Catalog, name string) (ToolDefinition, error) {
	if err := c.InternalOfferName(name); err != nil {
		return ToolDefinition{}, err
	}
	if m, ok := c.Model(name); ok {
		return oneShot(m.Name, "model", broker.MCPCommand())
	}
	if a, ok := c.Agent(name); ok {
		if a.Tool == fleet.OffersTool {
			return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "internal offer routing record"}
		}
		return oneShot(a.Name, "agent", chat.MCPAgentCommand())
	}
	if _, ok := c.Command(name); ok {
		cmd, err := c.RegisteredOfferCommand(name)
		if err != nil {
			return ToolDefinition{}, err
		}
		return toolDefinition(name, cmd)
	}
	toolName, command, qualified := strings.Cut(name, ":")
	if t, ok := c.Tool(toolName); ok && t.Name != fleet.OffersTool {
		if cmd, ok := t.Command(command); qualified && ok {
			return toolDefinition(name, cmd)
		}
		if !qualified && len(t.Commands) == 1 {
			return toolDefinition(name, t.Commands[0])
		}
		reason := "missing typed command args schema"
		if qualified {
			reason = "unknown tool command " + command
		} else if len(t.Commands) > 1 {
			reason = "select a tool:command"
		}
		return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: reason}
	}
	if !qualified {
		var match *fleet.ToolCommand
		tools, _ := c.Tools(true)
		for _, t := range tools {
			if t.Name == fleet.OffersTool {
				continue
			}
			if cmd, found := t.Command(name); found {
				if match != nil {
					return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "several tools declare this command; select a tool:command"}
				}
				match = &cmd
			}
		}
		if match != nil {
			return toolDefinition(name, *match)
		}
	}
	return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "no fleet model, agent, tool command or registered command"}
}

func oneShot(name, class string, cmd mcp.RegisteredCommand) (ToolDefinition, error) {
	var output *jsonschema.Schema
	var err error
	if class == "agent" {
		output, err = chat.MCPAgentOutputSchema()
		cmd.Usage = strings.ReplaceAll(cmd.Usage, "max-tokens", "token_cap")
	} else {
		output, err = jsonschema.For[broker.CallResult](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.RawMessage](): {}}})
	}
	if err != nil {
		return ToolDefinition{}, err
	}
	schema := *cmd.Schema
	schema.Flags = slices.Clone(schema.Flags)
	for i := range schema.Flags {
		f := &schema.Flags[i]
		if f.Name == class {
			f.Required = false
			f.Default = name
			f.Enum = []string{name}
		}
		if class == "agent" && f.Name == "max-tokens" {
			f.Name = "token_cap"
		}
	}
	return ToolDefinition{Name: name, Class: class, Description: cmd.Synopsis + " — " + cmd.Usage, Input: schema.JSONSchema(), Output: output, Effects: cmd.Effects, Unit: "token"}, nil
}

// OneShotArgv translates an offered call to its original M1 arguments. The
// target belongs to the offer and cannot be redirected by the caller.
func (d ToolDefinition) OneShotArgv(input map[string]any) ([]string, error) {
	var cmd mcp.RegisteredCommand
	args := make(map[string]any, len(input)+1)
	maps.Copy(args, input)
	switch d.Class {
	case "model":
		cmd = broker.MCPCommand()
	case "agent":
		cmd = chat.MCPAgentCommand()
		if _, ok := args["max-tokens"]; ok {
			return nil, fmt.Errorf("unknown argument max-tokens; use token_cap")
		}
		if cap, ok := args["token_cap"]; ok {
			args["max-tokens"] = cap
			delete(args, "token_cap")
		}
	default:
		return nil, fmt.Errorf("%s is not a one-shot model or agent", d.Name)
	}
	if target, ok := args[d.Class]; ok && target != d.Name {
		return nil, fmt.Errorf("%s is fixed to %s", d.Class, d.Name)
	}
	args[d.Class] = d.Name
	return cmd.Schema.Argv(args)
}

func toolDefinition(name string, cmd fleet.ToolCommand) (ToolDefinition, error) {
	var args tool.ArgSchema
	switch {
	case cmd.HTTP != nil && cmd.Mode == fleet.ToolCommandHTTP && len(cmd.HTTP.Args) > 0:
		if err := cmd.HTTP.Validate(cmd.Effects); err != nil {
			return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: err.Error()}
		}
		for _, a := range cmd.HTTP.Args {
			typ := map[string]string{"integer": "int", "boolean": "bool"}[a.Type]
			args.Flags = append(args.Flags, tool.ArgFlag{Name: a.Name, Type: typ, Required: a.Required, Default: a.Default, Enum: slices.Sorted(maps.Keys(a.Enum))})
		}
	case cmd.Mode == fleet.ToolCommandPrint && cmd.Args != nil:
		if err := cmd.Args.Validate(); err != nil {
			return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: err.Error()}
		}
		args = cmd.Args.ArgumentSchema()
	default:
		return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "missing typed args schema"}
	}
	unit := cmp.Or(cmd.Unit, "call")
	if unit != "call" && unit != "bytes" && unit != "seconds" {
		return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "invalid tool output unit " + unit}
	}
	desc := cmp.Or(cmd.Description, "Run "+name)
	input := args.JSONSchema()
	if cmd.HTTP != nil {
		props := input["properties"].(map[string]any)
		for _, arg := range cmd.HTTP.Args {
			prop, ok := props[arg.Name].(map[string]any)
			if !ok {
				return ToolDefinition{}, &fleet.OfferError{Name: name, Reason: "invalid HTTP argument schema for " + arg.Name}
			}
			limit := arg.MaxLen
			if limit == 0 {
				limit = fleet.ToolCommandArgMaxLen
			}
			prop["x-bashy-max-bytes"] = limit
			if arg.Type == fleet.ToolCommandArgJSONFile {
				prop["description"] = "Path to a JSON object file"
			}
		}
	}
	output, err := jsonschema.For[toolcmd.Result](nil)
	if err != nil {
		return ToolDefinition{}, err
	}
	d := ToolDefinition{
		Name: name, Class: "tool", Description: desc, Input: input, Output: output,
		Effects: append([]string{}, cmd.Effects...), Unit: unit,
	}
	return d.WithTags(cmd.Capability), nil
}
