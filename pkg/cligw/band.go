package cligw

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/recommend"
)

// SelectorKind records which request-model grammar form was parsed.
type SelectorKind string

const (
	SelectorBand  SelectorKind = "band"
	SelectorAuto  SelectorKind = "auto"
	SelectorModel SelectorKind = "model"
	SelectorAgent SelectorKind = "agent"
)

// Selector is a parsed request model. MinBand distinguishes L4+ from L4.
type Selector struct {
	Raw     string
	Kind    SelectorKind
	Band    int
	MinBand bool
	Name    string

	catalog *FleetCatalog
}

// UnknownSelectorError reports an unresolvable request model and the nearest
// names in the actual fleet catalog.
type UnknownSelectorError struct {
	Name        string
	Suggestions []string
}

func (e *UnknownSelectorError) Error() string {
	if len(e.Suggestions) == 0 {
		return fmt.Sprintf("cligw: unknown model selector %q", e.Name)
	}
	return fmt.Sprintf("cligw: unknown model selector %q; did you mean %s?", e.Name, strings.Join(e.Suggestions, ", "))
}

// ParseModelSelector parses and validates against the process's standard
// merged fleet registry.
func ParseModelSelector(s string) (Selector, error) {
	return LoadFleetCatalog().ParseModelSelector(s)
}

// ParseModelSelector parses in the documented precedence order: band, band+,
// auto, model/alias, then agent/tool:model.
func (c *FleetCatalog) ParseModelSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Selector{}, fmt.Errorf("cligw: model selector is empty")
	}
	if band, plus, ok := parseBand(s); ok {
		return Selector{Raw: s, Kind: SelectorBand, Band: band, MinBand: plus, catalog: c}, nil
	}
	if strings.EqualFold(s, "auto") {
		return Selector{Raw: s, Kind: SelectorAuto, catalog: c}, nil
	}
	if m, ok := c.fleet.Model(s); ok {
		return Selector{Raw: s, Kind: SelectorModel, Name: m.Name, catalog: c}, nil
	}
	if a, ok := c.fleet.Agent(s); ok {
		return Selector{Raw: s, Kind: SelectorAgent, Name: a.Name, catalog: c}, nil
	}
	return Selector{}, c.unknownSelector(s)
}

func parseBand(s string) (band int, plus bool, ok bool) {
	if len(s) < 2 || (s[0] != 'L' && s[0] != 'l') {
		return 0, false, false
	}
	rest := s[1:]
	if strings.HasSuffix(rest, "+") {
		plus, rest = true, strings.TrimSuffix(rest, "+")
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > fleet.MaxBand {
		return 0, false, false
	}
	return n, plus, true
}

// Classify resolves an auto selector from an OpenAI request body. Other
// selector forms are returned unchanged.
func (s Selector) Classify(body []byte, history resolve.PromptHistory) Selector {
	if s.Kind != SelectorAuto {
		return s
	}
	band, _, _ := resolve.ClassifyFromPrompt(body, history)
	if band < 1 {
		band = 1
	}
	if band > fleet.MaxBand {
		band = fleet.MaxBand
	}
	s.Kind, s.Band, s.MinBand = SelectorBand, band, false
	return s
}

// Candidates returns launchable agents admitted by sel and filter. L<N>+
// orders the lowest sufficient band first; every tie is agent-name sorted.
func (c *FleetCatalog) Candidates(ctx context.Context, sel Selector, filter Filter) []Agent {
	_ = ctx
	if sel.Kind == SelectorAuto {
		sel = sel.Classify(nil, nil)
	}
	all := c.filteredInventory(filter)
	out := make([]Agent, 0, len(all))
	for _, a := range all {
		switch sel.Kind {
		case SelectorBand:
			if sel.MinBand {
				if a.Band < sel.Band {
					continue
				}
			} else if a.Band != sel.Band {
				continue
			}
		case SelectorModel:
			if a.Model != sel.Name {
				continue
			}
		case SelectorAgent:
			if a.Name != sel.Name {
				continue
			}
		default:
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Band != out[j].Band {
			return out[i].Band < out[j].Band
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Candidates uses the catalog that parsed sel, falling back to the standard
// process catalog for a manually constructed Selector.
func Candidates(ctx context.Context, sel Selector, filter Filter) []Agent {
	cat := sel.catalog
	if cat == nil {
		cat = LoadFleetCatalog()
	}
	return cat.Candidates(ctx, sel, filter)
}

// CandidatesFromPrompt classifies auto from body before selecting candidates.
func (c *FleetCatalog) CandidatesFromPrompt(ctx context.Context, sel Selector, filter Filter, body []byte, history resolve.PromptHistory) []Agent {
	return c.Candidates(ctx, sel.Classify(body, history), filter)
}

// Filter is the intersection of operator defaults and X-Bashy-Filter fields.
// Empty fields do not constrain candidates.
type Filter struct {
	Kind       string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Provider   string `json:"provider,omitempty" yaml:"provider,omitempty"`
	Tool       string `json:"tool,omitempty" yaml:"tool,omitempty"`
	BandSource string `json:"band_source,omitempty" yaml:"band_source,omitempty"`
	// Slash is a canonical tool-command name (fleet.ToolCommand.Name, e.g.
	// "plan"): only agents whose tool declares that command match, and a
	// routed request carrying it RUNS the command (toolcmd) instead of a
	// tools-off completion. It is a per-request key — see slash.go.
	Slash string `json:"slash,omitempty" yaml:"slash,omitempty"`
}

// ParseFilter parses X-Bashy-Filter's key=value[,key=value] syntax.
func ParseFilter(header string) (Filter, error) {
	var out Filter
	if strings.TrimSpace(header) == "" {
		return out, nil
	}
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return Filter{}, fmt.Errorf("cligw: invalid filter %q (want key=value)", part)
		}
		switch key {
		case "kind":
			out.Kind = value
		case "provider":
			out.Provider = value
		case "tool":
			out.Tool = value
		case "band_source":
			out.BandSource = value
		case "slash":
			out.Slash = strings.TrimPrefix(value, "/")
		default:
			return Filter{}, fmt.Errorf("cligw: unknown filter key %q", key)
		}
	}
	return out, nil
}

// Merge overlays non-empty request fields on an operator config default.
func (f Filter) Merge(request Filter) Filter {
	if request.Kind != "" {
		f.Kind = request.Kind
	}
	if request.Provider != "" {
		f.Provider = request.Provider
	}
	if request.Tool != "" {
		f.Tool = request.Tool
	}
	if request.BandSource != "" {
		f.BandSource = request.BandSource
	}
	if request.Slash != "" {
		f.Slash = request.Slash
	}
	return f
}

// ParseFilterWithDefault parses a request header and overlays it on config.
func ParseFilterWithDefault(header string, config Filter) (Filter, error) {
	request, err := ParseFilter(header)
	if err != nil {
		return Filter{}, err
	}
	return config.Merge(request), nil
}

// Match reports whether an agent satisfies every non-empty field.
func (f Filter) Match(a Agent) bool {
	return (f.Kind == "" || a.Kind == f.Kind) &&
		(f.Provider == "" || a.Provider == f.Provider) &&
		(f.Tool == "" || a.Tool == f.Tool) &&
		(f.BandSource == "" || a.BandSource == f.BandSource) &&
		(f.Slash == "" || slices.Contains(a.Commands, f.Slash))
}

func (c *FleetCatalog) unknownSelector(name string) error {
	all := make([]string, 0, fleet.MaxBand*2)
	for band := 1; band <= fleet.MaxBand; band++ {
		all = append(all, fleet.BandLabel(band), fleet.BandLabel(band)+"+")
	}
	all = append(all, "auto")
	models, _ := c.fleet.Models()
	for _, m := range models {
		all = append(all, m.Names()...)
	}
	agents, _ := c.fleet.Agents()
	for _, a := range agents {
		all = append(all, a.Names()...)
		all = append(all, a.MatrixKey())
	}
	all = uniqueStrings(all)
	recs := recommend.Recommend(name, all, 3)
	suggestions := make([]string, 0, len(recs))
	for _, rec := range recs {
		suggestions = append(suggestions, rec.Name)
	}
	return &UnknownSelectorError{Name: name, Suggestions: suggestions}
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok || s == "" {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
