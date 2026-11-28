package offer

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/jsonschema-go/jsonschema"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/fleet"
)

func init() { broker.RegisterOfferRunner(runOffer) }

// runOffer uses K2's definition to bind one call and never accepts a caller
// supplied target. The broker supplies the authenticated principal.
func runOffer(ctx context.Context, rec broker.OfferRecord, args map[string]any) (json.RawMessage, any, error) {
	var d ToolDefinition
	if err := json.Unmarshal(rec.Definition, &d); err != nil {
		return nil, nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := validateInput(d, args); err != nil {
		return nil, nil, err
	}
	switch d.Class {
	case "model", "agent":
		var argv []string
		var err error
		if d.Class == "agent" {
			argv, err = agentOfferArgv(d, rec.TokenCap, args)
		} else {
			argv, err = d.OneShotArgv(args)
		}
		if err != nil {
			return nil, nil, err
		}
		cmd := broker.MCPCommand()
		if d.Class == "agent" {
			cmd = chat.MCPAgentCommand()
		}
		out, stderr, code := cmd.Run(ctx, argv, "", "")
		if code != 0 {
			return nil, nil, fmt.Errorf("offered %s failed: %s", d.Class, strings.TrimSpace(stderr))
		}
		var value json.RawMessage = []byte(out)
		if !json.Valid(value) {
			return nil, nil, fmt.Errorf("offered %s returned invalid JSON", d.Class)
		}
		var envelope struct {
			Usage any `json:"usage"`
		}
		_ = json.Unmarshal(value, &envelope)
		return value, envelope.Usage, nil
	case "tool":
		cat := fleet.New()
		toolName, command, qualified := strings.Cut(d.Name, ":")
		var t fleet.Tool
		var c fleet.ToolCommand
		var ok bool
		if _, found := cat.Command(d.Name); found {
			c, _ = cat.RegisteredOfferCommand(d.Name)
			t, ok = cat.Tool(fleet.OffersTool)
			if !ok {
				return nil, nil, fmt.Errorf("offers tool unavailable")
			}
		} else if qualified {
			t, ok = cat.Tool(toolName)
			if ok {
				c, ok = t.Command(command)
			}
		} else {
			t, ok = cat.Tool(d.Name)
			if ok && len(t.Commands) == 1 {
				c = t.Commands[0]
			} else {
				ok = false
			}
			if !ok {
				tools, _ := cat.Tools(true)
				for _, candidate := range tools {
					if candidate.Name == fleet.OffersTool {
						continue
					}
					if command, found := candidate.Command(d.Name); found {
						if ok {
							return nil, nil, fmt.Errorf("several tools declare %s", d.Name)
						}
						t, c, ok = candidate, command, true
					}
				}
			}
		}
		if !ok {
			return nil, nil, fmt.Errorf("offered command unavailable")
		}
		argv, err := toolArgs(d, args)
		if err != nil {
			return nil, nil, err
		}
		started := time.Now()
		result, err := cligw.RunOfferedTool(ctx, t, c, argv, cat)
		if err != nil {
			return nil, nil, err
		}
		value, err := json.Marshal(result)
		if err != nil {
			return nil, nil, err
		}
		usage := map[string]any{"calls": 1}
		if d.Unit == "bytes" {
			usage = map[string]any{"bytes": len(value)}
		}
		if d.Unit == "seconds" {
			usage = map[string]any{"seconds": int64(max(time.Second, time.Since(started)) / time.Second)}
		}
		return value, usage, nil
	}
	return nil, nil, fmt.Errorf("unknown offer class %q", d.Class)
}

func agentOfferArgv(d ToolDefinition, cap int64, args map[string]any) ([]string, error) {
	if cap <= 0 {
		return nil, fmt.Errorf("agent offer has no token cap")
	}
	if n, ok := args["token_cap"]; ok {
		var requested float64
		switch v := n.(type) {
		case float64:
			requested = v
		case int:
			requested = float64(v)
		case int64:
			requested = float64(v)
		default:
			return nil, fmt.Errorf("invalid token_cap")
		}
		if requested > float64(cap) || requested <= 0 {
			return nil, fmt.Errorf("token_cap exceeds offer cap")
		}
	}
	args["token_cap"] = cap
	return d.OneShotArgv(args)
}

func validateInput(d ToolDefinition, args map[string]any) error {
	raw, err := json.Marshal(d.Input)
	if err != nil {
		return err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	if err := resolved.Validate(args); err != nil {
		return fmt.Errorf("offer arguments: %w", err)
	}
	return nil
}

// toolArgs keeps the typed definition as the source of accepted argument names.
func toolArgs(d ToolDefinition, input map[string]any) ([]string, error) {
	props, _ := d.Input["properties"].(map[string]any)
	keys := make([]string, 0, len(input))
	for k := range input {
		if _, ok := props[k]; !ok {
			return nil, fmt.Errorf("unknown argument %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var argv []string
	for _, k := range keys {
		v := input[k]
		switch x := v.(type) {
		case string:
			argv = append(argv, "--"+k, x)
		case float64, bool:
			argv = append(argv, "--"+k, fmt.Sprint(x))
		default:
			return nil, fmt.Errorf("invalid argument %q", k)
		}
	}
	return argv, nil
}
