package recall

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/redact"
)

// TestRecall_NeverTouchesTheFactStore is the most important test in this package
// and it is a SOURCE-LEVEL assertion on purpose.
//
// A behavioural test ("facts do not appear in output") only proves the code path
// exercised by that test does not print facts. The invariant is stronger: recall
// must have no way to reach facts at all, because its output is piped into
// software we do not control, and printing facts would make it the export path
// the fact store's whole design exists to deny (skill-graph-design.md
// §Invariant 3 — the ABSENCE of a marshaller is the enforcement).
//
// So this parses the package's own AST and fails on any reference to craft's fact
// API. It is deliberately blunt: the diff that erodes this boundary looks
// reasonable ("just add --include-facts for debugging"), and a test that only
// checks output would pass right up until someone piped it somewhere.
func TestRecall_NeverTouchesTheFactStore(t *testing.T) {
	isolateRecallStores(t)
	banned := []string{"OpenFacts", "FactStore", "Fact"}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "craft" {
				return true
			}
			for _, b := range banned {
				if sel.Sel.Name == b {
					t.Errorf("%s references craft.%s — recall MUST NOT be able to reach the fact store; "+
						"if you need entity-scoped detail, promote it to a FOLD through pkg/redact",
						fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files were checked — the guard is not actually running")
	}
}

// TestRecall_PerRingCapsAreNotGlobal pins §5's decision. A global cap would force
// cross-ring score comparison, which was measured and rejected (RRF with a
// no-signal arm dropped MRR 0.594 -> 0.417 at identical hit@3).
func TestRecall_PerRingCapsAreNotGlobal(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kbWith(t, dir,
		page("stuck-process-one", "stopping a stuck process", "how to stop a stuck process safely"),
		page("stuck-process-two", "stuck process on a host", "another take on a stuck process"),
		page("stuck-process-three", "process stuck at exit", "a third stuck process note"),
	)
	folds := foldsWith(t, dir,
		craft.Fold{Coordinate: "c1", Note: "a stuck process needs the tool's own stop verb"},
		craft.Fold{Coordinate: "c2", Note: "stuck process: never kill by pattern"},
	)

	res := Recall(Query{Text: "stuck process", K: 2, Now: time.Now()},
		HostRing{Store: store}, CapabilityRing{Folds: folds})

	var host, cap int
	for _, h := range res.Hits {
		switch h.Ring {
		case RingHost:
			host++
		case RingCapability:
			cap++
		}
	}
	if host != 2 {
		t.Errorf("host ring returned %d hits, want 2 (K applies per ring)", host)
	}
	if cap != 2 {
		t.Errorf("capability ring returned %d hits, want 2 (K applies per ring)", cap)
	}
	if len(res.Hits) != 4 {
		t.Errorf("total = %d, want 4 — K=2 is PER RING, so a global cap of 2 is a bug", len(res.Hits))
	}
}

// TestRecall_RingsAreIndependent — one unreadable ring must not suppress another's
// answer, and it must be REPORTED rather than swallowed. A caller has to be able
// to tell "nothing known" from "something broke".
func TestRecall_RingsAreIndependent(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kbWith(t, dir, page("known", "a known lesson", "something we know about widgets"))

	res := Recall(Query{Text: "widgets"}, HostRing{Store: store}, brokenRing{})

	if len(res.Hits) == 0 {
		t.Error("a broken ring suppressed a healthy ring's answer")
	}
	if len(res.Errors) != 1 || res.Errors[0].Ring != "broken" {
		t.Errorf("errors = %+v, want the broken ring recorded", res.Errors)
	}
}

// TestRecall_EmptyIsAnAnswerNotAnError — the exit-code contract's core. Nothing
// known is information; conflating it with a failure is how an agent proceeds on
// a false negative.
func TestRecall_EmptyIsAnAnswerNotAnError(t *testing.T) {
	isolateRecallStores(t)
	store := kbWith(t, t.TempDir(), page("unrelated", "kubernetes ingress", "how ingress works"))
	res := Recall(Query{Text: "zzzz nonexistent topic qqqq"}, HostRing{Store: store})
	if len(res.Hits) != 0 {
		t.Fatalf("expected no hits, got %d", len(res.Hits))
	}
	if len(res.Errors) != 0 {
		t.Errorf("an empty answer produced errors %+v — empty is exit 0", res.Errors)
	}
	if res.Abstained {
		t.Error("abstained without MinCoverage set; abstention must be opt-in")
	}
}

// TestRecall_AbstainsWhenAsked — abstention is a first-class answer, not an error.
func TestRecall_AbstainsWhenAsked(t *testing.T) {
	isolateRecallStores(t)
	store := kbWith(t, t.TempDir(), page("unrelated", "kubernetes ingress", "how ingress works"))
	res := Recall(Query{Text: "zzzz nonexistent qqqq", MinCoverage: 0.9}, HostRing{Store: store})
	if !res.Abstained {
		t.Error("MinCoverage 0.9 on an uncovered query did not abstain")
	}
	if len(res.Errors) != 0 {
		t.Errorf("abstention is not an error, got %+v", res.Errors)
	}
}

// TestRecall_BudgetTruncatesAtAHitBoundary — a half-rendered record is worse than
// one fewer record, because it can be quoted as if complete.
func TestRecall_BudgetTruncatesAtAHitBoundary(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kbWith(t, dir,
		page("widget-one", "widget handling one", "the first thing to know about widget handling"),
		page("widget-two", "widget handling two", "the second thing to know about widget handling"),
		page("widget-three", "widget handling three", "the third thing about widget handling"),
	)
	full := Recall(Query{Text: "widget handling", K: 5}, HostRing{Store: store})
	if len(full.Hits) < 2 {
		t.Fatalf("need >=2 hits to test truncation, got %d", len(full.Hits))
	}
	// A budget between one and two hits must keep exactly one, whole.
	budget := tokens(full.Hits[0]) + 1
	got := Recall(Query{Text: "widget handling", K: 5, Budget: budget}, HostRing{Store: store})
	if len(got.Hits) != 1 {
		t.Errorf("hits = %d, want 1 whole hit under a 1.x-hit budget", len(got.Hits))
	}
	if !got.Budget.Truncated {
		t.Error("truncation was not reported; a caller cannot tell it got a partial answer")
	}
	if got.Budget.Spent > budget {
		t.Errorf("spent %d over budget %d", got.Budget.Spent, budget)
	}
}

// TestRecall_EveryHitCarriesRingAndSource — both are contract fields. A hit with
// no re-openable source is a reconstruction, and provenance is what stops a
// third party from treating a candidate note as policy.
func TestRecall_EveryHitCarriesRingAndSource(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kbWith(t, dir, page("widget", "widget lesson", "a lesson about widgets"))
	folds := foldsWith(t, dir, craft.Fold{Coordinate: "c1", Note: "widgets need a restart"})
	res := Recall(Query{Text: "widget"}, HostRing{Store: store}, CapabilityRing{Folds: folds})
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range res.Hits {
		if h.Ring == "" {
			t.Errorf("hit %q has no ring", h.ID)
		}
		if len(h.Source) == 0 || h.Source[0].URI == "" {
			t.Errorf("hit %q has no source pointer — it would be a reconstruction", h.ID)
		}
		if len(h.Why) == 0 {
			t.Errorf("hit %q does not explain itself; `why` is part of the contract", h.ID)
		}
	}
}

// TestRecall_DeterministicOrder — a caller building on an unstable order has no
// reproducible behaviour.
func TestRecall_DeterministicOrder(t *testing.T) {
	isolateRecallStores(t)
	dir := t.TempDir()
	store := kbWith(t, dir,
		page("a-widget", "widget alpha", "widget alpha notes"),
		page("b-widget", "widget beta", "widget beta notes"),
	)
	first := Recall(Query{Text: "widget", K: 5}, HostRing{Store: store})
	for i := 0; i < 5; i++ {
		again := Recall(Query{Text: "widget", K: 5}, HostRing{Store: store})
		if len(again.Hits) != len(first.Hits) {
			t.Fatalf("hit count changed between runs: %d vs %d", len(first.Hits), len(again.Hits))
		}
		for j := range first.Hits {
			if first.Hits[j].ID != again.Hits[j].ID {
				t.Fatalf("order changed at %d: %q vs %q", j, first.Hits[j].ID, again.Hits[j].ID)
			}
		}
	}
}

// --- helpers ---

type brokenRing struct{}

func (brokenRing) Ring() string    { return "broken" }
func (brokenRing) Forms() []string { return []string{kb.FormPage} }
func (brokenRing) Recall(Query) ([]Hit, error) {
	return nil, os.ErrPermission
}

func page(slug, title, desc string) *kb.Page {
	return &kb.Page{Slug: slug, Type: kb.TypeLesson, Title: title,
		Description: desc, Status: kb.StatusValidated}
}

func kbWith(t *testing.T, dir string, pages ...*kb.Page) *kb.Store {
	t.Helper()
	root := filepath.Join(dir, "kb")
	s := kb.Open(root)
	for _, p := range pages {
		if err := s.Write(p, "add"); err != nil {
			t.Fatalf("write %s: %v", p.Slug, err)
		}
	}
	return s
}

func foldsWith(t *testing.T, dir string, folds ...craft.Fold) *craft.FoldStore {
	t.Helper()
	s := craft.OpenFolds(filepath.Join(dir, "craft"), redact.New())
	now := time.Now()
	for _, f := range folds {
		if f.ObservedAt.IsZero() {
			f.ObservedAt = now
		}
		if f.ValidFrom.IsZero() {
			f.ValidFrom = now
		}
		if err := s.Record(f); err != nil {
			t.Fatalf("record fold: %v", err)
		}
	}
	return s
}
