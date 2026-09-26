package gateway

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

// GET /v1/models — the OpenAI list shape, so stock clients (openai-python,
// Cline, sgpt, …) accept it without an adapter. Everything the gateway adds
// beyond the standard fields is `x_`-prefixed, which strict OpenAI
// validators ignore and richer clients can pick up.

// ModelKindLocal marks an entry served by a pooled backend — the cost and
// privacy floor. Hosts contributing remote entries through ListExtra set
// their own kind (conventionally "api") and provider.
const ModelKindLocal = "local"

// ModelEntry is one entry in the /v1/models list.
//
// XBackends is populated only when the caller passes ?expand=hosts, because
// the per-backend expansion reveals the shape of the caller's pool. The
// default list is de-duplicated by name, mirroring the bare OpenAI shape.
// Its JSON key stays `x_hosts`: the field was renamed to backend vocabulary,
// the wire contract was not.
type ModelEntry struct {
	ID            string         `json:"id"`
	Object        string         `json:"object"`
	Created       int64          `json:"created"`
	OwnedBy       string         `json:"owned_by"`
	XCapabilities []string       `json:"x_capabilities,omitempty"`
	XContextLen   int64          `json:"x_context_length,omitempty"`
	XBackends     []ModelBackend `json:"x_hosts,omitempty"`

	// XKind and XProvider let a client tell a pooled model from a remote
	// one: XKind is ModelKindLocal for catalog entries, XProvider names
	// the vendor for remote entries and is empty for local ones.
	XKind     string `json:"x_kind,omitempty"`
	XProvider string `json:"x_provider,omitempty"`

	// XCluster describes a distributed-inference cluster serving this
	// model — a model split across several machines that none could hold
	// alone. The resolver's catalog carries no cluster topology, so the
	// gateway never fills this; a host does, through DecorateModel.
	XCluster *ModelCluster `json:"x_cluster,omitempty"`
}

// ModelCluster is the aggregate view of a distributed-inference cluster.
type ModelCluster struct {
	Backend       string `json:"backend,omitempty"`
	MemberCount   int    `json:"member_count,omitempty"`
	MaxModelBytes int64  `json:"max_model_bytes,omitempty"`
}

// ModelBackend is one backend's contribution to a model, so a client can
// reason about pool topology (render an "available on N of M" badge, say).
// The JSON keys are the established wire names.
type ModelBackend struct {
	Host         string `json:"host"`
	Owner        string `json:"owner"`
	Digest       string `json:"digest,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	MaxParallel  int    `json:"max_parallel,omitempty"`
}

// ModelList is the {object: "list", data: [...]} wrapper every OpenAI list
// endpoint returns.
type ModelList struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// listModels serves GET /v1/models. It aggregates the catalog rows the
// principal can reach, de-duplicates by model name (the same model on N
// backends shows once — the scheduler picks a backend per request), appends
// whatever ListExtra contributes, filters by AllowModel, and sorts by id.
//
// ?expand=hosts attaches the per-backend x_hosts array.
func (g *gateway) listModels(w http.ResponseWriter, r *http.Request) {
	principal, _, err := g.cfg.Authorize(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody(err.Error()))
		return
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		writeJSON(w, http.StatusUnauthorized, errBody("no principal"))
		return
	}
	expandBackends := wantExpandHosts(r.URL.Query().Get("expand"))
	ctx := r.Context()

	var rows []resolve.ModelRow
	if g.cfg.Catalog != nil {
		rows = g.cfg.Catalog.Rows(ctx, principal)
	}

	// First row per name wins as the canonical entry — catalogs return
	// rows in priority order, so that is the principal's own backend.
	order := make([]string, 0, len(rows))
	entries := make(map[string]ModelEntry, len(rows))
	rowsByName := make(map[string][]resolve.ModelRow, len(rows))
	for _, row := range rows {
		rowsByName[row.Name] = append(rowsByName[row.Name], row)
		if _, exists := entries[row.Name]; exists {
			continue
		}
		order = append(order, row.Name)
		entries[row.Name] = ModelEntry{
			ID:            row.Name,
			Object:        "model",
			Created:       row.UpdatedAt.Unix(),
			OwnedBy:       ModelKindLocal,
			XCapabilities: row.Capabilities,
			XContextLen:   row.ContextLen,
			XKind:         ModelKindLocal,
		}
	}

	// Host-contributed entries. A catalog entry always wins a name
	// collision: the pooled model is never shadowed by a remote row of
	// the same name.
	if g.cfg.ListExtra != nil {
		for _, extra := range g.cfg.ListExtra(ctx, principal) {
			if _, exists := entries[extra.ID]; exists {
				continue
			}
			if extra.Object == "" {
				extra.Object = "model"
			}
			order = append(order, extra.ID)
			entries[extra.ID] = extra
		}
	}

	data := make([]ModelEntry, 0, len(order))
	for _, name := range order {
		if g.cfg.AllowModel != nil && !g.cfg.AllowModel(principal, name) {
			continue
		}
		entry := entries[name]
		if expandBackends {
			entry.XBackends = g.backendDetail(rowsByName[name])
		}
		if g.cfg.DecorateModel != nil {
			entry = g.cfg.DecorateModel(rowsByName[name], entry)
		}
		data = append(data, entry)
	}
	// Stable order by id so callers can diff and paginate sanely.
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

	writeJSON(w, http.StatusOK, ModelList{Object: "list", Data: data})
}

// backendDetail renders the x_hosts expansion for one model's rows. The
// resolver's ModelRow carries no owner, digest or quantization, so those
// stay empty unless a host fills them through DecorateModel; max_parallel
// comes from the capacity cache when a fresh observation exists.
func (g *gateway) backendDetail(rows []resolve.ModelRow) []ModelBackend {
	seen := map[string]struct{}{}
	out := make([]ModelBackend, 0, len(rows))
	for _, row := range rows {
		if _, dup := seen[row.Backend]; dup {
			continue
		}
		seen[row.Backend] = struct{}{}
		detail := ModelBackend{Host: row.Backend}
		if capacity, ok := g.cfg.Capacity.Get(row.Backend); ok {
			detail.MaxParallel = capacity.MaxParallel
		}
		out = append(out, detail)
	}
	return out
}

// wantExpandHosts parses the comma-separated `expand` query parameter,
// mirroring the REST convention. The only expansion recognised today is
// "hosts"; a future one chains with a comma.
func wantExpandHosts(raw string) bool {
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(strings.ToLower(part)) == "hosts" {
			return true
		}
	}
	return false
}

// ListExtraModels is a convenience for hosts whose extra entries are a
// static slice: it adapts one to a ListExtraFunc.
func ListExtraModels(entries []ModelEntry) ListExtraFunc {
	return func(context.Context, string) []ModelEntry { return entries }
}
