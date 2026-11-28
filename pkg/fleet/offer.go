package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/qiangli/bash/interp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

const (
	OffersTool  = "offers"
	offersModel = "registered-commands"
	offersAgent = "registered-offers"
)

// OfferError reports the record or field that made an offer ineligible.
type OfferError struct{ Name, Reason string }

func (e *OfferError) Error() string { return fmt.Sprintf("cannot offer %q: %s", e.Name, e.Reason) }

// InternalOfferName identifies command surfaces owned by the shell. Embedders
// supply their additional mounted names through WithReservedNames.
func (c *Catalog) InternalOfferName(name string) error {
	reason := ""
	switch {
	case name == OffersTool || name == offersModel || name == offersAgent:
		reason = "internal offer routing record"
	case slices.Contains([]string{"bash", "bashy", "bash#", "bash++", "bashsharp", "bashpp"}, strings.ToLower(name)), interp.IsBuiltin(name):
		reason = "internal shell verb or interpreter"
	case slices.Contains(atlas.GNUCoreutilsUpstream(), name):
		reason = "coreutils command"
	default:
		if _, ok := atlas.Lookup(name); ok || slices.Contains(reservedCommandWords, name) {
			reason = "internal bashy command"
		} else if c.cfg.reservedName != nil {
			if holder, taken := c.cfg.reservedName(name); taken {
				reason = "internal command: " + holder
			}
		}
	}
	if reason != "" {
		return &OfferError{name, reason}
	}
	return nil
}

// ArgumentSchema projects the record through the same schema used by direct MCP tools.
func (s CommandSchema) ArgumentSchema() tool.ArgSchema {
	var out tool.ArgSchema
	for _, p := range s.Positionals {
		p.Default = normalizeOfferDefault(p.Type, p.Default)
		out.Positionals = append(out.Positionals, tool.ArgParameter(p))
	}
	for _, f := range s.Flags {
		f.Default = normalizeOfferDefault(f.Type, f.Default)
		out.Flags = append(out.Flags, tool.ArgFlag(f))
	}
	return out
}

func normalizeOfferDefault(typ, text string) string {
	if text != "" {
		if v, err := canonicalArgValue(typ, text); err == nil {
			return v
		}
	}
	return text
}

// RegisteredOfferCommand materializes the slash runner contract for one record.
func (c *Catalog) RegisteredOfferCommand(name string) (ToolCommand, error) {
	if err := c.InternalOfferName(name); err != nil {
		return ToolCommand{}, err
	}
	r, ok := c.Command(name)
	if !ok {
		return ToolCommand{}, &OfferError{name, "no registered command"}
	}
	if err := c.InternalOfferName(r.Name); err != nil {
		return ToolCommand{}, err
	}
	if strings.HasPrefix(r.Name, "-") || strings.ContainsAny(r.Name, "{}") || len(strings.Fields(r.Name)) != 1 {
		return ToolCommand{}, &OfferError{name, "invalid exec command name"}
	}
	if err := r.Validate(c.cfg.reservedName); err != nil {
		return ToolCommand{}, &OfferError{name, err.Error()}
	}
	if r.Args == nil {
		return ToolCommand{}, &OfferError{name, "missing typed args schema"}
	}
	return ToolCommand{Name: name, Description: r.Synopsis, Args: r.Args, Mode: ToolCommandPrint, Slash: name + " {args}", Exec: "bashy " + r.Name + " {args}", Effects: r.Effects, Timeout: r.Timeout, Unit: r.Unit, Output: r.Output}, nil
}

// SetRegisteredOffers replaces the generated overlay with the complete selected
// set. Validation finishes before the single atomic tool write.
func (c *Catalog) SetRegisteredOffers(names []string) error {
	t, ok := c.Tool(OffersTool)
	if !ok || t.Binary() != "bashy" {
		return fmt.Errorf("fleet: missing bashy offers tool")
	}
	t.Commands = nil
	for _, name := range slices.Compact(slices.Sorted(slices.Values(names))) {
		cmd, err := c.RegisteredOfferCommand(name)
		if err != nil {
			return err
		}
		t.Commands = append(t.Commands, cmd)
	}
	// Create the ordinary binding only on hosts that publish a command.
	// Writing commands last leaves an interrupted first publication inert.
	if len(t.Commands) > 0 {
		m, found := c.Model(offersModel)
		if found && (m.Name != offersModel || m.Provider != OffersTool) {
			return fmt.Errorf("fleet: offer model name %q already in use", offersModel)
		}
		a, ok := c.Agent(offersAgent)
		if ok && (a.Name != offersAgent || a.Tool != OffersTool || a.Model != offersModel) {
			return fmt.Errorf("fleet: offer agent name %q already in use", offersAgent)
		}
		if !found {
			if err := c.SaveModel(Model{Name: offersModel, Kind: ModelKindLocal, Source: ModelSourceLocal, Provider: OffersTool}); err != nil {
				return err
			}
		}
		if !ok {
			if err := c.SaveAgent(Agent{Name: offersAgent, Tool: OffersTool, Model: offersModel}); err != nil {
				return err
			}
		}
	}
	return c.saveChanged(dirTools, t.Name, t, t, []string{"commands"})
}

// OfferArgv renders a JSON argument object as literal CLI operands. The shell
// binds the record again at dispatch. Use explicit boolean values and terminate
// the flag list before positionals, including operands that look like flags.
func (s CommandSchema) OfferArgv(raw string) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var input map[string]any
	if err := dec.Decode(&input); err != nil || input == nil {
		return nil, fmt.Errorf("expected a JSON argument object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after arguments")
	}
	schema := s.ArgumentSchema()
	// Each flag is encoded explicitly (including false). Let the shared binder
	// normalize the JSON scalar before adding its CLI spelling.
	flags := schema.Flags
	schema.Flags = nil
	argv := []string{}
	for _, f := range flags {
		value, present := input[f.Name]
		if !present {
			if f.Required {
				return nil, fmt.Errorf("missing required argument %q", f.Name)
			}
			continue
		}
		param := tool.ArgParameter{Name: f.Name, Type: f.Type, Required: true, Enum: f.Enum}
		flagInput := map[string]any{f.Name: value}
		normalized, err := (tool.ArgSchema{Positionals: []tool.ArgParameter{param}}).Argv(flagInput)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--"+f.Name+"="+normalized[0])
		delete(input, f.Name)
	}
	operands, err := schema.Argv(input)
	if err != nil {
		return nil, err
	}
	if len(operands) > 0 {
		argv = append(argv, "--")
		argv = append(argv, operands...)
	}
	return argv, nil
}

// IsOffers reports whether t is the hidden tool that runs offered registered
// commands on the door. It keeps that one mechanism check in the fleet
// package, so the gateway carries no tool-name branch.
func (t Tool) IsOffers() bool { return t.Name == OffersTool }
