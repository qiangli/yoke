package cligw

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

// FleetCatalog projects bashy's merged fleet registry onto the gateway's
// model catalog. The candidate unit is an agent, not a model: two tools bound
// to the same model are two independently routable backends.
//
// The projection is CACHED for inventoryTTL. Deriving it resolves every
// agent's tool/model binding through the fleet catalog — measured at ~0.8s for
// a 125-agent fleet — and it sits on the path of every routing decision, every
// /v1/models row and every autoscaler tick. Without the cache a single model
// listing re-derived it once per row and took minutes.
type FleetCatalog struct {
	fleet *fleet.Catalog

	mu       sync.Mutex
	cached   []Agent
	index    map[string]Agent
	cachedAt time.Time
}

// inventoryTTL is how long a derived inventory is reused. A registry edit is a
// human-scale event, so a second of staleness costs nothing next to
// re-deriving the whole binding table hundreds of times a minute.
const inventoryTTL = 5 * time.Second

// NewFleetCatalog wraps an already configured fleet catalog. Tests and
// embedders use this to supply an isolated ring.
func NewFleetCatalog(cat *fleet.Catalog) *FleetCatalog {
	if cat == nil {
		cat = fleet.New()
	}
	return &FleetCatalog{fleet: cat}
}

// LoadFleetCatalog loads the standard embedded, shared, cloud, and local
// fleet rings using the same entry point as the bashy model/agent commands.
func LoadFleetCatalog() *FleetCatalog { return NewFleetCatalog(fleet.New()) }

// Registry returns the underlying merged fleet catalog.
func (c *FleetCatalog) Registry() *fleet.Catalog { return c.fleet }

// Rows implements resolve.Catalog. principal is intentionally ignored: cligw
// serves the local operator's fleet and request-level narrowing is expressed
// by Filter, not by an implicit subscription or provider policy.
func (c *FleetCatalog) Rows(_ context.Context, _ string) []resolve.ModelRow {
	agents := c.inventory()
	rows := make([]resolve.ModelRow, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, resolve.ModelRow{
			Name:         a.Model,
			Backend:      a.Name,
			Capabilities: cloneStrings(a.Capabilities),
			ContextLen:   a.ContextLength,
			Class:        a.Band,
			Domains:      cloneStrings(a.Domains),
		})
	}
	return rows
}

// Alias implements resolve.Catalog. It exposes fleet model aliases (including
// derived family aliases) and exact band aliases to the shared resolver.
func (c *FleetCatalog) Alias(_ context.Context, name string) (int, []string, bool) {
	if band, plus, ok := parseBand(name); ok && !plus {
		return band, cloneStrings(resolve.AllDomains), true
	}
	models, _ := c.fleet.Models()
	for _, m := range models {
		for _, alias := range m.Names()[1:] {
			if alias == name {
				return m.Band, modelDomains(m), true
			}
		}
	}
	return 0, nil, false
}

// Agent is the gateway's flattened view of one launchable fleet agent.
type Agent struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	// ModelID is the provider-side id the tool is handed for this binding,
	// when it differs from Model (the registry name). The door's admission
	// preview guards a coord claim on either.
	ModelID       string   `json:"model_id,omitempty"`
	Tool          string   `json:"tool"`
	Band          int      `json:"band"`
	BandSource    string   `json:"band_source"`
	Kind          string   `json:"kind"`
	Provider      string   `json:"provider"`
	Warm          string   `json:"warm"`
	Effort        string   `json:"effort,omitempty"` // declared reasoning effort; "" = the tool's default
	Capabilities  []string `json:"capabilities,omitempty"`
	Domains       []string `json:"domains,omitempty"`
	ContextLength int64    `json:"context_length,omitempty"`
	// Commands are the canonical names of the tool commands the agent's
	// tool declares (fleet.Tool.Commands), name-sorted. Filter.Slash
	// matches against them.
	Commands []string `json:"commands,omitempty"`
}

// ModelEntry is one OpenAI model-list row plus bashy routing metadata.
// XQuota is reserved for L8 and is deliberately empty here.
type ModelEntry struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by"`
	XBand       int    `json:"x_band"`
	XBandSource string `json:"x_band_source"`
	XKind       string `json:"x_kind"`
	XProvider   string `json:"x_provider"`
	XTool       string `json:"x_tool"`
	XWarm       string `json:"x_warm"`
	XQuota      string `json:"x_quota"`
	// XCommands lists the tool commands the row's agent declares; a
	// ?slash=NAME / X-Bashy-Filter: slash=NAME listing keeps only rows
	// whose agent declares NAME.
	XCommands []string `json:"x_commands,omitempty"`
}

// ModelList returns band aliases first, then launchable registry models and
// agents. Every section is deterministic and independently name-sorted.
func (c *FleetCatalog) ModelList(ctx context.Context, filter Filter) []ModelEntry {
	_ = ctx
	inv := c.filteredInventory(filter)
	out := make([]ModelEntry, 0, fleet.MaxBand*2+len(inv)*2)
	for band := 1; band <= fleet.MaxBand; band++ {
		out = append(out, bandEntry(band, false))
	}
	for band := 1; band <= fleet.MaxBand; band++ {
		out = append(out, bandEntry(band, true))
	}

	// A registry model has a CLI pool iff at least one filtered, launchable
	// agent exposes it. The first agent is deterministic and supplies tool/warm
	// metadata for the aggregate model row.
	byModel := make(map[string]Agent, len(inv))
	for _, a := range inv {
		if _, exists := byModel[a.Model]; !exists {
			byModel[a.Model] = a
		}
	}
	modelNames := make([]string, 0, len(byModel))
	for name := range byModel {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		a := byModel[name]
		out = append(out, modelEntry(name, a))
	}
	for _, a := range inv {
		out = append(out, modelEntry(a.Name, a))
	}
	return out
}

// ModelList uses the process's standard merged fleet catalog.
func ModelList(ctx context.Context, filter Filter) []ModelEntry {
	return LoadFleetCatalog().ModelList(ctx, filter)
}

func bandEntry(band int, plus bool) ModelEntry {
	id := fleet.BandLabel(band)
	if plus {
		id += "+"
	}
	return ModelEntry{ID: id, Object: "model", OwnedBy: "bashy", XBand: band, XWarm: "cold"}
}

func modelEntry(id string, a Agent) ModelEntry {
	return ModelEntry{
		ID: id, Object: "model", OwnedBy: "bashy",
		XBand: a.Band, XBandSource: a.BandSource, XKind: a.Kind,
		XProvider: a.Provider, XTool: a.Tool, XWarm: a.Warm,
		XCommands: cloneOrNil(a.Commands),
	}
}

// inventory returns the cached agent projection. The returned slice and the
// string slices inside it are SHARED with every other caller — read them, do
// not sort or mutate them in place.
func (c *FleetCatalog) inventory() []Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < inventoryTTL {
		return c.cached
	}
	return c.refreshLocked()
}

func (c *FleetCatalog) refreshLocked() []Agent {
	c.cached = c.deriveInventory()
	c.index = make(map[string]Agent, len(c.cached))
	for _, agent := range c.cached {
		c.index[agent.Name] = agent
	}
	c.cachedAt = time.Now()
	return c.cached
}

// Agent returns one launchable agent from the cached projection. It is the O(1)
// lookup the HTTP surface needs: a scan per model-list row is what made the
// uncached listing quadratic.
func (c *FleetCatalog) Agent(name string) (Agent, bool) {
	c.inventory()
	c.mu.Lock()
	defer c.mu.Unlock()
	agent, ok := c.index[name]
	if !ok {
		c.refreshLocked()
		agent, ok = c.index[name]
	}
	return agent, ok
}

func (c *FleetCatalog) deriveInventory() []Agent {
	rows, _ := c.fleet.Agents()
	out := make([]Agent, 0, len(rows))
	for _, raw := range rows {
		// Match the default `bashy agent list`: task-local clones are not roster
		// entries, while every persistent agent is eligible regardless of kind.
		if raw.Ephemeral || raw.IsRetired() || raw.Unavailable != "" {
			continue
		}
		a, tool, model, err := c.fleet.Binding(raw.Name)
		if err != nil || !hasLaunchContract(tool) {
			continue
		}
		band, source := model.Band, effectiveBandSource(model.Band, model.BandSource)
		if a.IsCascade() && a.Band > 0 {
			band, source = a.Band, fleet.BandCascade
		}
		modelID := model.TargetFor(tool.Name)
		if modelID == model.Name {
			modelID = ""
		}
		out = append(out, Agent{
			Name: a.Name, Model: model.Name, ModelID: modelID, Tool: tool.Name,
			Band: band, BandSource: source, Kind: model.Kind,
			Provider: model.Provider, Warm: warmMode(tool), Effort: a.Effort,
			Capabilities: cloneStrings(model.Capabilities),
			Domains:      modelDomains(model), ContextLength: model.ContextLength,
			Commands: commandNames(tool),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *FleetCatalog) filteredInventory(filter Filter) []Agent {
	all := c.inventory()
	out := make([]Agent, 0, len(all))
	for _, a := range all {
		if filter.Match(a) {
			out = append(out, a)
		}
	}
	return out
}

func modelDomains(m fleet.Model) []string {
	if len(m.Domain) > 0 {
		return cloneStrings(m.Domain)
	}
	_, domains := resolve.ClassifyModel(m.Name, m.Family, "", m.Capabilities)
	return domains
}

func effectiveBandSource(band int, source string) string {
	if band > 0 && strings.TrimSpace(source) == "" {
		return fleet.BandDeclared
	}
	return source
}

func hasLaunchContract(t fleet.Tool) bool {
	l := t.CLI.Launch
	return strings.TrimSpace(l.Exec) != "" || strings.TrimSpace(l.ACPExec) != ""
}

func warmMode(t fleet.Tool) string {
	if mode := reflectedString(t.CLI.Launch, "Warm"); mode != "" {
		return mode
	}
	return "cold"
}

// reflectedString lets L7 consume the optional launch.warm field both before
// and after the independently delivered L5 fleet schema change is merged.
func reflectedString(v any, field string) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	f := rv.FieldByName(field)
	if f.IsValid() && f.Kind() == reflect.String {
		return strings.TrimSpace(f.String())
	}
	return ""
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }

func cloneOrNil(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return cloneStrings(in)
}

// commandNames is the sorted, de-duplicated set of a tool's declared
// command names.
func commandNames(t fleet.Tool) []string {
	if len(t.Commands) == 0 {
		return nil
	}
	names := make([]string, 0, len(t.Commands))
	for _, c := range t.Commands {
		if name := strings.TrimSpace(c.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

var _ resolve.Catalog = (*FleetCatalog)(nil)
