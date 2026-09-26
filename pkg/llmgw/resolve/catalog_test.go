package resolve

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// fakeCatalog is the test double for the one seam the resolver has. It
// counts Rows calls so the "at most one catalog read per resolution"
// contract stays honest.
type fakeCatalog struct {
	rows     map[string][]ModelRow
	aliases  map[string]fakeAlias
	rowCalls int
}

type fakeAlias struct {
	class   int
	domains []string
}

func (c *fakeCatalog) Rows(_ context.Context, principal string) []ModelRow {
	c.rowCalls++
	return c.rows[principal]
}

func (c *fakeCatalog) Alias(_ context.Context, name string) (int, []string, bool) {
	a, ok := c.aliases[name]
	if !ok {
		return 0, nil, false
	}
	return a.class, a.domains, true
}

const testPrincipal = "principal-a"

// newTestCatalog returns a small inventory: two L2 coding models (one of
// them on two backends), one L2 general model, one L4 general model.
func newTestCatalog() *fakeCatalog {
	now := time.Now().UTC()
	return &fakeCatalog{
		rows: map[string][]ModelRow{
			testPrincipal: {
				{Name: "qwen2.5-coder:7b", Backend: "b1", Class: TierL2, Domains: []string{"coding"}, UpdatedAt: now},
				{Name: "deepseek-coder:6.7b", Backend: "b2", Class: TierL2, Domains: []string{"coding"}, UpdatedAt: now},
				{Name: "deepseek-coder:6.7b", Backend: "b3", Class: TierL2, Domains: []string{"coding"}, Loaded: true, UpdatedAt: now},
				{Name: "llama3.1:8b", Backend: "b2", Class: TierL2, Domains: []string{"general"}, UpdatedAt: now},
				{Name: "llama3.1:70b", Backend: "b4", Class: TierL4, Domains: []string{"general"}, UpdatedAt: now},
			},
		},
		aliases: map[string]fakeAlias{
			"best-coder": {class: TierL2, domains: []string{"coding"}},
			"no-domains": {class: TierL2},
		},
	}
}

func autoReq(model string) Request {
	return Request{Model: model, AutoEnabled: true, Mode: ModePreferExact}
}

func TestCandidateModels(t *testing.T) {
	codingBody := []byte(`{"messages":[{"role":"user","content":"write a func in Go"}]}`)

	for _, tt := range []struct {
		name string
		req  Request
		body []byte
		want []string
	}{
		{
			name: "auto off - exact only",
			req:  Request{Model: "qwen2.5-coder:7b"},
			want: []string{"qwen2.5-coder:7b"},
		},
		{
			name: "empty model short-circuits",
			req:  Request{Model: "", AutoEnabled: true, Mode: ModePreferExact},
			want: []string{""},
		},
		{
			name: "concrete name widens to its class, exact first",
			req:  autoReq("qwen2.5-coder:7b"),
			want: []string{"qwen2.5-coder:7b", "deepseek-coder:6.7b"},
		},
		{
			name: "unknown name classified from the name itself",
			req:  autoReq("mistral-nemo:12b"),
			want: []string{"mistral-nemo:12b", "llama3.1:8b"},
		},
		{
			name: "no reachable row in class falls back to exact",
			req:  autoReq("some-embed-model:1b"),
			want: []string{"some-embed-model:1b"},
		},
		{
			name: "capability spec",
			req:  autoReq("tier:L4/general"),
			want: []string{"llama3.1:70b"},
		},
		{
			name: "capability spec with no match resolves to nothing",
			req:  autoReq("tier:L3/coding"),
			want: nil,
		},
		{
			name: "capability spec wildcard takes every domain in the class",
			req:  autoReq("tier:L2/*"),
			want: []string{"qwen2.5-coder:7b", "deepseek-coder:6.7b", "llama3.1:8b"},
		},
		{
			name: "alias",
			req:  autoReq("best-coder"),
			want: []string{"qwen2.5-coder:7b", "deepseek-coder:6.7b"},
		},
		{
			name: "alias without domains falls through to name classification",
			req:  autoReq("no-domains"),
			// The alias hit carries no domain, so it is ignored and
			// the name itself is classified: (L2, general).
			want: []string{"no-domains", "llama3.1:8b"},
		},
		{
			name: "auto model name classifies from the prompt",
			req:  autoReq("auto"),
			body: codingBody,
			want: []string{"qwen2.5-coder:7b", "deepseek-coder:6.7b"},
		},
		{
			name: "auto model name with no match resolves to nothing",
			req:  autoReq("auto"),
			body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			// Heuristic says (L2, general) → llama3.1:8b.
			want: []string{"llama3.1:8b"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := newTestCatalog()
			got := CandidateModels(context.Background(), cat, tt.req, testPrincipal, nil, nil, tt.body)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("CandidateModels = %v, want %v", got, tt.want)
			}
			if cat.rowCalls > 1 {
				t.Errorf("catalog Rows called %d times, want at most 1", cat.rowCalls)
			}
		})
	}
}

// The alias branch resolves to nothing when no reachable row is in the
// alias's class — an alias names a class, so falling back to the alias
// string as a model name would dispatch something nobody asked for.
func TestCandidateModels_AliasWithNoReachableRow(t *testing.T) {
	cat := newTestCatalog()
	cat.aliases["best-embed"] = fakeAlias{class: TierL1, domains: []string{"embedding"}}
	got := CandidateModels(context.Background(), cat, autoReq("best-embed"), testPrincipal, nil, nil, nil)
	if got != nil {
		t.Fatalf("CandidateModels = %v, want nil", got)
	}
}

// A nil catalog is a resolver with no inventory: the class-addressed
// forms resolve to nothing, a concrete name still routes to itself.
func TestCandidateModels_NilCatalog(t *testing.T) {
	if got := CandidateModels(context.Background(), nil, autoReq("qwen2.5-coder:7b"), testPrincipal, nil, nil, nil); !reflect.DeepEqual(got, []string{"qwen2.5-coder:7b"}) {
		t.Errorf("concrete name with nil catalog = %v, want the exact model", got)
	}
	if got := CandidateModels(context.Background(), nil, autoReq("tier:L2/coding"), testPrincipal, nil, nil, nil); got != nil {
		t.Errorf("capability spec with nil catalog = %v, want nil", got)
	}
}

// The history stage of the classifier chain reaches CandidateModels
// through the "auto" model name.
func TestCandidateModels_AutoUsesPromptHistory(t *testing.T) {
	cat := newTestCatalog()
	body := []byte(`{"messages":[{"role":"user","content":"write a func in Go"}]}`)
	hist := PromptHistoryFunc(func(string) (PromptResolution, bool) {
		return PromptResolution{Tier: TierL4, Domains: []string{"general"}, Success: true}, true
	})
	got := CandidateModels(context.Background(), cat, autoReq("auto"), testPrincipal, nil, hist, body)
	if !reflect.DeepEqual(got, []string{"llama3.1:70b"}) {
		t.Fatalf("CandidateModels with history = %v, want [llama3.1:70b]", got)
	}
}

func TestCandidateModels_BestWarm(t *testing.T) {
	cat := newTestCatalog()
	req := Request{Model: "qwen2.5-coder:7b", AutoEnabled: true, Mode: ModeBestWarm}

	// b3 carries deepseek and is warm; the exact model's only backend
	// is cold — so the substitute leapfrogs the exact match.
	loaded := func(backend, model string) bool { return backend == "b3" }
	got := CandidateModels(context.Background(), cat, req, testPrincipal, loaded, nil, nil)
	want := []string{"deepseek-coder:6.7b", "qwen2.5-coder:7b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("best-warm = %v, want %v", got, want)
	}

	// Everything cold → the prefer-exact order survives.
	cold := func(string, string) bool { return false }
	got = CandidateModels(context.Background(), cat, req, testPrincipal, cold, nil, nil)
	want = []string{"qwen2.5-coder:7b", "deepseek-coder:6.7b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("best-warm all cold = %v, want %v", got, want)
	}

	// No residency signal at all → no re-ordering, not "all cold".
	got = CandidateModels(context.Background(), cat, req, testPrincipal, nil, nil, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("best-warm with nil LoadedFunc = %v, want %v", got, want)
	}
}

// Candidate order follows catalog order, which is how a host expresses
// "the principal's own backends first".
func TestCandidateModels_PreservesCatalogOrder(t *testing.T) {
	cat := &fakeCatalog{rows: map[string][]ModelRow{
		testPrincipal: {
			{Name: "own-coder:7b", Backend: "own", Class: TierL2, Domains: []string{"coding"}},
			{Name: "shared-coder:7b", Backend: "shared", Class: TierL2, Domains: []string{"coding"}},
		},
	}}
	got := CandidateModels(context.Background(), cat, autoReq("tier:L2/coding"), testPrincipal, nil, nil, nil)
	want := []string{"own-coder:7b", "shared-coder:7b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CandidateModels = %v, want %v", got, want)
	}
}

func TestFilterInClass(t *testing.T) {
	rows := newTestCatalog().rows[testPrincipal]

	if got := filterInClass(rows, TierL2, nil); got != nil {
		t.Errorf("empty domain filter matched %v, want nothing", got)
	}
	if got := uniqueModelNames(filterInClass(rows, 0, []string{"general"})); !reflect.DeepEqual(got, []string{"llama3.1:8b", "llama3.1:70b"}) {
		t.Errorf("class 0 should disable the class filter, got %v", got)
	}
	if got := uniqueModelNames(filterInClass(rows, TierL2, []string{"coding", "general"})); len(got) != 3 {
		t.Errorf("multi-domain filter = %v, want 3 names", got)
	}
	// A row the catalog never classified is never in a class-filtered set.
	unclassified := []ModelRow{{Name: "mystery", Backend: "b1", Domains: []string{"coding"}}}
	if got := filterInClass(unclassified, TierL2, []string{"coding"}); got != nil {
		t.Errorf("unclassified row matched a class query: %v", got)
	}
	if got := uniqueModelNames(filterInClass(unclassified, 0, []string{"coding"})); !reflect.DeepEqual(got, []string{"mystery"}) {
		t.Errorf("unclassified row should still match a classless query, got %v", got)
	}
}

func TestBackendsExposing(t *testing.T) {
	rows := newTestCatalog().rows[testPrincipal]
	if got := BackendsExposing(rows, "deepseek-coder:6.7b"); !reflect.DeepEqual(got, []string{"b2", "b3"}) {
		t.Errorf("BackendsExposing = %v, want [b2 b3]", got)
	}
	if got := BackendsExposing(rows, "nope"); len(got) != 0 {
		t.Errorf("BackendsExposing(unknown) = %v, want empty", got)
	}
	// Duplicate (backend, model) rows collapse.
	dup := []ModelRow{{Name: "m", Backend: "b1"}, {Name: "m", Backend: "b1"}}
	if got := BackendsExposing(dup, "m"); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Errorf("BackendsExposing(dup) = %v, want [b1]", got)
	}
}

func TestLookupAliasClass(t *testing.T) {
	cat := newTestCatalog()
	class, domains, ok := LookupAliasClass(context.Background(), cat, "best-coder")
	if !ok || class != TierL2 || !reflect.DeepEqual(domains, []string{"coding"}) {
		t.Fatalf("LookupAliasClass = (%d, %v, %v), want (2, [coding], true)", class, domains, ok)
	}
	if _, _, ok := LookupAliasClass(context.Background(), cat, "qwen2.5-coder:7b"); ok {
		t.Error("a plain model name must not resolve as an alias")
	}
	if _, _, ok := LookupAliasClass(context.Background(), nil, "best-coder"); ok {
		t.Error("nil catalog must not resolve an alias")
	}
}

func TestSplitCSVField(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"coding,general", []string{"coding", "general"}},
		{" coding , general ", []string{"coding", "general"}},
		{"coding,,general", []string{"coding", "general"}},
		{"", nil},
		{"   ", nil},
	} {
		if got := SplitCSVField(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitCSVField(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
