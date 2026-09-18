package capability

import (
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
)

// A matrix row is keyed by tool:model, so renaming a model to a
// version-explicit name strands its row and leaves its replacement missing.
// Load repairs both, because a stranded row still gets ranked by Best() — it
// would route work to a model that no longer exists — and a missing row makes
// a live agent invisible to the router.
func TestReconcileRetiresStaleRowsAndSeedsNewOnes(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI,
		Harness: map[string]float64{"operability": 0.95}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "opus4.8", Family: "opus", Version: "4.8", Band: 3, Quality: 0.92,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "claude-opus4.8", Tool: "claude", Model: "opus4.8"}); err != nil {
		t.Fatal(err)
	}

	prev := newCatalog
	newCatalog = func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	t.Cleanup(func() { newCatalog = prev })

	// A matrix written before the rename: the old key, plus one for a model
	// that is simply gone.
	m := &Matrix{Agents: map[string]map[Capability]Cell{
		"claude:opus":    {CapCoding: {Quality: 0.9, Source: "observed", Samples: 7}},
		"claude:retired": {CapCoding: {Quality: 0.5}},
	}}

	if !m.reconcile() {
		t.Fatal("reconcile must report that it changed something")
	}

	// `claude:opus` still RESOLVES — the family alias carries it to opus4.8 —
	// but it is no longer the canonical key, so it must not survive as a
	// duplicate row splitting the evidence for one agent across two names.
	if _, ok := m.Agents["claude:opus"]; ok {
		t.Error("the pre-rename key must be retired, not kept as a duplicate")
	}
	if _, ok := m.Agents["claude:retired"]; ok {
		t.Error("a row for a model that no longer exists must be dropped")
	}
	if _, ok := m.Agents["claude:opus4.8"]; !ok {
		t.Error("the canonical binding must be seeded so the agent stays routable")
	}
	if len(m.Agents) != 1 {
		t.Errorf("want exactly the one live binding, got %v", m.Agents)
	}
}

// Reconciling a matrix that already matches the catalog must be a no-op, or
// every Load would rewrite the file and clobber accumulated posteriors.
func TestReconcileIsIdempotent(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "opus4.8", Band: 3, Quality: 0.92}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "a", Tool: "claude", Model: "opus4.8"}); err != nil {
		t.Fatal(err)
	}
	prev := newCatalog
	newCatalog = func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	t.Cleanup(func() { newCatalog = prev })

	m := seedPriorsFrom(newCatalog())
	if m.reconcile() {
		t.Error("a matrix already in step with the catalog must not be rewritten")
	}

	// An observed posterior must survive a reconcile — it is the whole point
	// of persisting the matrix.
	m.Agents["claude:opus4.8"][CapCoding] = Cell{Quality: 0.99, Source: "observed", Samples: 12}
	if m.reconcile() {
		t.Error("reconcile must not disturb a matrix whose keys are all canonical")
	}
	if got := m.Agents["claude:opus4.8"][CapCoding]; got.Quality != 0.99 || got.Samples != 12 {
		t.Errorf("observed evidence was lost: %+v", got)
	}
}

func TestReconcileRetainsEvidenceForBindingSharedByNamedAgents(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "opus4.8", Band: 3, Quality: 0.92}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manager", "reviewer"} {
		if err := cat.SaveAgent(fleet.Agent{Name: name, Tool: "claude", Model: "opus4.8"}); err != nil {
			t.Fatal(err)
		}
	}
	prev := newCatalog
	newCatalog = func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	t.Cleanup(func() { newCatalog = prev })

	want := Cell{Quality: 0.99, Source: SourceHost, Samples: 12}
	m := &Matrix{Agents: map[string]map[Capability]Cell{
		"claude:opus4.8": {CapCoding: want},
	}}
	if m.reconcile() {
		t.Fatal("a shared binding is canonical capability evidence, not an ambiguous identity")
	}
	if got := m.Agents["claude:opus4.8"][CapCoding]; got != want {
		t.Fatalf("shared-binding evidence changed: got %+v want %+v", got, want)
	}
}
