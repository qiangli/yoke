package weave

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// THE JOIN. A sprint stores a manager's NAME and only a name — deliberately,
// because pinning a binding into the sprint would rot the moment the agent is
// re-bound. So `sprint status` could report WHO holds a seat and never WHAT
// that manager is, and answering "which sprints are active and who manages
// them" took two commands and a shell loop.
//
// Every assertion below is about a rule rather than about coverage: the join
// reuses the catalog, an unresolvable holder is a finding, and resolution
// neither probes nor writes.
func fixtureCatalog(t *testing.T) (*fleet.Catalog, string) {
	t.Helper()
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	if err := cat.SaveModel(fleet.Model{Name: "opus5", Band: 4, Kind: "subscription", Provider: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "seated", Tool: "claude", Model: "opus5"}); err != nil {
		t.Fatal(err)
	}
	return cat, root
}

func TestResolveManagerJoinsTheSeatToItsFleetRecord(t *testing.T) {
	cat, _ := fixtureCatalog(t)

	got := resolveManager(cat, "seated")
	if got == nil || !got.Resolved {
		t.Fatalf("resolveManager(seated) = %+v, want a resolved record", got)
	}
	if got.Tool != "claude" || got.Model != "opus5" || got.Binding != "claude:opus5" {
		t.Errorf("binding = %q (%s/%s)", got.Binding, got.Tool, got.Model)
	}
	// The band is the MODEL's peg, inherited. An agent never carries its own.
	if got.Band != 4 {
		t.Errorf("band = %d, want the model's peg 4", got.Band)
	}
}

// AN UNRESOLVABLE HOLDER IS A FINDING, NOT A BLANK.
//
// This is the state an operator most needs to see: the name was mistyped, the
// agent was removed, or the seat was taken by something nothing can push to.
// The row must keep its NAME and say the join failed — dropping it would make
// an unaddressable sprint look like an unowned one, which is a different
// problem with a different fix.
func TestResolveManagerReportsAnUnknownHolderRatherThanBlanking(t *testing.T) {
	cat, _ := fixtureCatalog(t)

	got := resolveManager(cat, "ghost")
	if got == nil {
		t.Fatal("resolveManager(ghost) = nil; an unknown holder must still be reported")
	}
	if got.Resolved {
		t.Errorf("ghost resolved = true, want false")
	}
	if got.Name != "ghost" {
		t.Errorf("name = %q, want the holder's name kept intact", got.Name)
	}
	if got.Reason == "" {
		t.Error("no reason given; an unresolved join must say why")
	}
	if l := got.label(); !strings.Contains(l, "UNRESOLVED") {
		t.Errorf("label = %q, want it to name the failure", l)
	}

	// An UNOWNED sprint has no holder at all, which is not the same thing.
	if resolveManager(cat, "") != nil || resolveManager(cat, "   ") != nil {
		t.Error("an empty holder produced a record; unowned is not unresolved")
	}
}

// RESOLUTION IS A CATALOG READ. It must not probe — `sprint tick` already
// states the cost, a probe is a real headless turn per row — and it must not
// write, because `sprint status` reports and changes nothing and resolution
// must never become a liveness signal for a seat nobody is driving.
//
// Asserted as a property of the STORE rather than by mocking a prober: nothing
// under the catalog root may change, appear or disappear.
func TestResolveManagerNeitherProbesNorWrites(t *testing.T) {
	cat, root := fixtureCatalog(t)

	before := treeOf(t, root)
	for _, name := range []string{"seated", "ghost", "claude:opus5", ""} {
		_ = resolveManager(cat, name)
	}
	if after := treeOf(t, root); !equalTrees(before, after) {
		t.Errorf("the catalog changed during resolution:\nbefore %v\nafter  %v", before, after)
	}
}

// THE LABEL is the text view's one-line answer, and it says which of the three
// states a row is in without the reader decoding anything.
func TestManagerLabelNamesTheBindingAndBand(t *testing.T) {
	cat, _ := fixtureCatalog(t)

	l := resolveManager(cat, "seated").label()
	if !strings.Contains(l, "claude:opus5") || !strings.Contains(l, "L4") {
		t.Errorf("label = %q, want the binding and the band", l)
	}
	// A nil record is an UNOWNED sprint and contributes nothing to the line.
	var none *managerAgent
	if l := none.label(); l != "" {
		t.Errorf("nil label = %q, want empty", l)
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if info.IsDir() {
			out = append(out, rel+"/")
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel+":"+string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func equalTrees(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
