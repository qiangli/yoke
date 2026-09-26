package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

func modelRows() []resolve.ModelRow {
	return []resolve.ModelRow{
		{
			Name:         "llama3.2:1b",
			Backend:      "alpha",
			Capabilities: []string{"completion", "tools"},
			ContextLen:   8192,
			Class:        resolve.TierL1,
			Domains:      []string{resolve.DomainGeneral},
			UpdatedAt:    time.Unix(1700000000, 0),
		},
		// Same model on a second backend: the list de-duplicates by
		// name, ?expand=hosts brings both back.
		{
			Name:       "llama3.2:1b",
			Backend:    "beta",
			ContextLen: 8192,
			UpdatedAt:  time.Unix(1700000100, 0),
		},
		{
			Name:         "codellama:7b",
			Backend:      "beta",
			Capabilities: []string{"completion"},
			ContextLen:   16384,
			Class:        resolve.TierL2,
			Domains:      []string{resolve.DomainCoding},
			UpdatedAt:    time.Unix(1700000200, 0),
		},
	}
}

func decodeList(t *testing.T, body []byte) ModelList {
	t.Helper()
	var list ModelList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return list
}

func TestListModels_Shape(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, nil)

	w := env.do(t, http.MethodGet, ModelsPath, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	list := decodeList(t, w.Body.Bytes())
	if list.Object != "list" {
		t.Errorf("object=%q, want list", list.Object)
	}
	if len(list.Data) != 2 {
		t.Fatalf("data=%d entries, want 2 (deduped by name): %+v", len(list.Data), list.Data)
	}
	// Sorted by id ascending.
	if list.Data[0].ID != "codellama:7b" || list.Data[1].ID != "llama3.2:1b" {
		t.Fatalf("ids=%q,%q — want sorted", list.Data[0].ID, list.Data[1].ID)
	}
	entry := list.Data[1]
	if entry.Object != "model" {
		t.Errorf("object=%q, want model", entry.Object)
	}
	if entry.Created != 1700000000 {
		t.Errorf("created=%d", entry.Created)
	}
	if entry.OwnedBy != ModelKindLocal || entry.XKind != ModelKindLocal {
		t.Errorf("owned_by=%q x_kind=%q", entry.OwnedBy, entry.XKind)
	}
	if entry.XContextLen != 8192 {
		t.Errorf("x_context_length=%d", entry.XContextLen)
	}
	if len(entry.XCapabilities) != 2 {
		t.Errorf("x_capabilities=%v", entry.XCapabilities)
	}
	if entry.XBackends != nil {
		t.Errorf("x_hosts must be absent without ?expand=hosts: %v", entry.XBackends)
	}

	// The x_ fields are the wire keys clients match on.
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	for _, key := range []string{"id", "object", "created", "owned_by", "x_capabilities", "x_context_length", "x_kind"} {
		if _, ok := raw.Data[1][key]; !ok {
			t.Errorf("entry missing JSON key %q: %v", key, raw.Data[1])
		}
	}
	if _, ok := raw.Data[1]["x_hosts"]; ok {
		t.Errorf("x_hosts present without expand")
	}
}

func TestListModels_ExpandHosts(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, nil)
	env.cfg.Capacity.Put("alpha", Capacity{MaxParallel: 3, InFlight: 1})

	w := env.do(t, http.MethodGet, ModelsPath+"?expand=hosts", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	list := decodeList(t, w.Body.Bytes())
	var entry ModelEntry
	for _, e := range list.Data {
		if e.ID == "llama3.2:1b" {
			entry = e
		}
	}
	if len(entry.XBackends) != 2 {
		t.Fatalf("x_hosts=%+v, want two backends", entry.XBackends)
	}
	if entry.XBackends[0].Host != "alpha" || entry.XBackends[1].Host != "beta" {
		t.Errorf("x_hosts order=%+v", entry.XBackends)
	}
	if entry.XBackends[0].MaxParallel != 3 {
		t.Errorf("max_parallel=%d, want the cached capacity", entry.XBackends[0].MaxParallel)
	}

	// The JSON key stays "host" even though the field is Backend-named.
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	hosts, _ := raw.Data[1]["x_hosts"].([]any)
	if len(hosts) == 0 {
		t.Fatalf("x_hosts missing: %v", raw.Data[1])
	}
	first, _ := hosts[0].(map[string]any)
	if _, ok := first["host"]; !ok {
		t.Errorf("x_hosts entry missing the host key: %v", first)
	}
}

func TestListModels_ExtraAndCollision(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, func(c *Config) {
		c.ListExtra = ListExtraModels([]ModelEntry{
			// Collides with a catalog entry: must not shadow it.
			{ID: "llama3.2:1b", OwnedBy: "vendor", XKind: "api", XProvider: "vendor"},
			{ID: "vendor-premium", OwnedBy: "vendor", XKind: "api", XProvider: "vendor"},
		})
	})
	w := env.do(t, http.MethodGet, ModelsPath, nil, nil)
	list := decodeList(t, w.Body.Bytes())
	if len(list.Data) != 3 {
		t.Fatalf("data=%d, want 3: %+v", len(list.Data), list.Data)
	}
	byID := map[string]ModelEntry{}
	for _, e := range list.Data {
		byID[e.ID] = e
	}
	if byID["llama3.2:1b"].XKind != ModelKindLocal {
		t.Errorf("a catalog entry must win a name collision: %+v", byID["llama3.2:1b"])
	}
	extra := byID["vendor-premium"]
	if extra.Object != "model" {
		t.Errorf("object defaulted wrong: %q", extra.Object)
	}
	if extra.XProvider != "vendor" {
		t.Errorf("x_provider=%q", extra.XProvider)
	}
}

func TestListModels_AllowModelFilters(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, func(c *Config) {
		c.AllowModel = func(_, model string) bool { return model == "codellama:7b" }
	})
	list := decodeList(t, env.do(t, http.MethodGet, ModelsPath, nil, nil).Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].ID != "codellama:7b" {
		t.Fatalf("data=%+v, want only the allowed model", list.Data)
	}
}

func TestListModels_DecorateFillsCatalogGaps(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, func(c *Config) {
		c.DecorateModel = func(rows []resolve.ModelRow, entry ModelEntry) ModelEntry {
			entry.OwnedBy = "operator"
			if len(rows) > 1 {
				entry.XCluster = &ModelCluster{Backend: "distributed", MemberCount: len(rows)}
			}
			return entry
		}
	})
	list := decodeList(t, env.do(t, http.MethodGet, ModelsPath, nil, nil).Body.Bytes())
	for _, e := range list.Data {
		if e.OwnedBy != "operator" {
			t.Errorf("%s owned_by=%q", e.ID, e.OwnedBy)
		}
	}
	var clustered *ModelCluster
	for _, e := range list.Data {
		if e.ID == "llama3.2:1b" {
			clustered = e.XCluster
		}
	}
	if clustered == nil || clustered.MemberCount != 2 {
		t.Fatalf("x_cluster=%+v", clustered)
	}
}

func TestListModels_EmptyCatalogIsEmptyList(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, nil, []*upstream{up}, nil)
	w := env.do(t, http.MethodGet, ModelsPath, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	list := decodeList(t, w.Body.Bytes())
	if list.Object != "list" || len(list.Data) != 0 {
		t.Errorf("list=%+v, want an empty list", list)
	}
}

func TestListModels_AliasPrefix(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{up}, nil)
	w := env.do(t, http.MethodGet, AliasPrefix+ModelsPath, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if len(decodeList(t, w.Body.Bytes()).Data) != 2 {
		t.Errorf("alias prefix served a different list")
	}
}

func TestWantExpandHosts(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"hosts", true},
		{"HOSTS", true},
		{" usage , hosts ", true},
		{"usage", false},
	} {
		if got := wantExpandHosts(tc.raw); got != tc.want {
			t.Errorf("wantExpandHosts(%q)=%v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestHealth_ReportsBreakerAndCapacity(t *testing.T) {
	alpha := newUpstream(t, "alpha", echoJSON)
	beta := newUpstream(t, "beta", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{alpha, beta}, nil)
	env.cfg.Capacity.Put("alpha", Capacity{MaxParallel: 4, InFlight: 2})
	env.cfg.Breaker.Trip("beta", time.Minute)

	w := env.do(t, http.MethodGet, HealthPath, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var report HealthReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if report.Status != "ok" {
		t.Errorf("status=%q, want ok (alpha is available)", report.Status)
	}
	if len(report.Backends) != 2 {
		t.Fatalf("backends=%+v", report.Backends)
	}
	a, b := report.Backends[0], report.Backends[1]
	if a.Name != "alpha" || a.Cooling {
		t.Errorf("alpha=%+v", a)
	}
	if !a.CapacityFresh || a.Capacity == nil || a.Capacity.MaxParallel != 4 {
		t.Errorf("alpha capacity=%+v fresh=%v", a.Capacity, a.CapacityFresh)
	}
	// Loaded is json:"-" — the derived ratio is recomputed by a reader,
	// not carried on the wire; in_flight and max_parallel are.
	if a.Capacity.InFlight != 2 {
		t.Errorf("alpha in_flight=%d, want 2", a.Capacity.InFlight)
	}
	if b.Name != "beta" || !b.Cooling {
		t.Errorf("beta=%+v, want cooling", b)
	}
	if b.CapacityFresh || b.Capacity != nil {
		t.Errorf("beta capacity should be absent: %+v", b.Capacity)
	}
}

func TestHealth_AllCoolingIsDegraded(t *testing.T) {
	alpha := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{alpha}, nil)
	env.cfg.Breaker.Trip("alpha", time.Minute)

	var report HealthReport
	w := env.do(t, http.MethodGet, HealthPath, nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Status != "degraded" {
		t.Errorf("status=%q, want degraded", report.Status)
	}
}

func TestHealth_NoBackendsHookIsOK(t *testing.T) {
	alpha := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{alpha}, func(c *Config) { c.Backends = nil })
	w := env.do(t, http.MethodGet, HealthPath, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var report HealthReport
	_ = json.Unmarshal(w.Body.Bytes(), &report)
	if report.Status != "ok" || len(report.Backends) != 0 {
		t.Errorf("report=%+v", report)
	}
}

// Health does not authenticate — it is the operator's liveness surface,
// and it names no principal's models.
func TestHealth_NeedsNoPrincipal(t *testing.T) {
	alpha := newUpstream(t, "alpha", echoJSON)
	env := newEnv(t, modelRows(), []*upstream{alpha}, func(c *Config) {
		c.Authorize = func(*http.Request) (string, int, error) { return "", 0, errors.New("nope") }
	})
	if w := env.do(t, http.MethodGet, HealthPath, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if w := env.do(t, http.MethodGet, AliasPrefix+HealthPath, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("alias status=%d, want 200", w.Code)
	}
}

func TestListExtraModels(t *testing.T) {
	entries := []ModelEntry{{ID: "x"}}
	if got := ListExtraModels(entries)(context.Background(), "alice"); len(got) != 1 || got[0].ID != "x" {
		t.Errorf("ListExtraModels=%+v", got)
	}
}
