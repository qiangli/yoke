package capability

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/seed_priors.golden from the current baseline")

// bare builds a catalog with an EMPTY baseline and a scratch local store, so a
// test sees exactly the entries it wrote and nothing else.
func bare(root string) *fleet.Catalog {
	return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
}

// The capability priors moved out of Go literals and into the fleet registry.
// This pins every cell they produced, so the move is provably value-preserving
// and any future edit to a baseline YAML shows up as an intentional diff.
func TestSeedPriorsMatchGolden(t *testing.T) {
	fleettest.Ring(t)
	prev := newCatalog
	newCatalog = func() *fleet.Catalog {
		// Embedded tools + the test ring, empty local store: the golden values
		// ARE the baseline.
		return fleet.New(fleet.WithRoot(t.TempDir()))
	}
	t.Cleanup(func() { newCatalog = prev })

	m := seedPriors()
	var got []string
	for agent, row := range m.Agents {
		for c, cell := range row {
			got = append(got, fmt.Sprintf("%s|%s|%.4f|%d", agent, c, cell.Quality, cell.CostMicro))
		}
	}
	sort.Strings(got)

	// Re-pegging a band or a quality prior legitimately moves these cells.
	// `go test ./pkg/capability -update` rewrites the golden so the diff shows
	// up in review as an intentional change rather than a chore.
	if *updateGolden {
		if err := os.WriteFile("testdata/seed_priors.golden",
			[]byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote testdata/seed_priors.golden")
		return
	}

	raw, err := os.ReadFile("testdata/seed_priors.golden")
	if err != nil {
		t.Fatal(err)
	}
	// Normalize CRLF: git's autocrlf checks the golden out with \r\n on
	// Windows, and a whole-string TrimSpace leaves the per-line \r behind.
	norm := strings.ReplaceAll(string(raw), "\r\n", "\n")
	want := strings.Split(strings.TrimSpace(norm), "\n")
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("cell %d:\n  got  %s\n  want %s", i, got[i], want[i])
		}
	}
}

// Two nicknames for one binding must collapse to one row. Fragmenting the
// matrix by nickname would split the evidence the router accumulates.
func TestNicknamesDoNotFragmentTheMatrix(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI,
		Harness: map[string]float64{"operability": 0.95}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "opus", Quality: 0.92, CostMicro: 15000}); err != nil {
		t.Fatal(err)
	}
	for _, nick := range []string{"007", "bond"} {
		if err := cat.SaveAgent(fleet.Agent{Name: nick, Tool: "claude", Model: "opus"}); err != nil {
			t.Fatal(err)
		}
	}
	m := seedPriorsFrom(bare(root))
	if len(m.Agents) != 1 {
		t.Fatalf("matrix has %d rows, want 1 (both nicknames name claude:opus)", len(m.Agents))
	}
	if _, ok := m.Agents["claude:opus"]; !ok {
		t.Fatalf("rows keyed by nickname instead of binding: %v", m.Agents)
	}
}

// A model added to the registry is routable without editing Go.
func TestNewModelIsRoutableWithoutCodeChange(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveModel(fleet.Model{
		Name: "brandnew", Kind: fleet.ModelKindAPI, APIKeyRef: "k",
		UpstreamID: "vendor/brandnew", Quality: 0.88, CostMicro: 999,
		Spec: map[string]float64{"coding": 0.05},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "nova", Tool: "claude", Model: "brandnew"}); err != nil {
		t.Fatal(err)
	}
	// Real baseline (for claude's harness scores) + this scratch local store.
	m := seedPriorsFrom(fleet.New(fleet.WithRoot(root)))
	row, ok := m.Agents["claude:brandnew"]
	if !ok {
		t.Fatalf("new binding absent: %v", m.Agents)
	}
	if got := row[CapCoding]; got.Quality < 0.929 || got.Quality > 0.931 || got.CostMicro != 999 {
		t.Fatalf("coding cell = %+v, want quality 0.93 (0.88 tier + 0.05 spec), cost 999", got)
	}
	// Harness comes from the TOOL, not the model.
	if got := row[CapOperability]; got.Quality != 0.95 {
		t.Fatalf("operability = %v, want claude's harness score 0.95", got.Quality)
	}
}
