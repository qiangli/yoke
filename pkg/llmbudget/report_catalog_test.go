package llmbudget

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func isolateReportCatalog(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BASHY_") {
			t.Setenv(key, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}
func TestReportEmptyModelDoesNotExpandCatalog(t *testing.T) {
	isolateReportCatalog(t)
	g := New(Config{Models: map[string]Model{"known": {Name: "known"}}})
	if _, ok := g.model(""); ok {
		t.Fatal("empty source model resolved")
	}
	// Empty provider-source rows used to parse every fleet YAML file on each
	// report. A zero-allocation lookup guards that material performance bug.
	allocations := testing.AllocsPerRun(5, func() {
		if _, ok := g.model(""); ok {
			panic("empty model resolved")
		}
	})
	if allocations != 0 {
		t.Fatalf("empty model loaded catalog: %.0f allocations", allocations)
	}
}
func TestReportCatalogRefreshesAliasesAndUnknowns(t *testing.T) {
	isolateReportCatalog(t)
	cat := fleet.New()
	model := fleet.Model{Name: "report-fixture-model", Aliases: []string{"report-fixture-alias"}, Provider: "first-vendor", Kind: "api"}
	if e := cat.SaveModel(model); e != nil {
		t.Fatal(e)
	}
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "budget.json"), Policy: &Policy{Version: 1}, Models: map[string]Model{}})
	read := func() string {
		r, e := g.CollectReport(context.Background(), ReportOptions{Roster: []Binding{{Model: "report-fixture-alias"}}})
		if e != nil {
			t.Fatal(e)
		}
		for _, account := range r.Accounts {
			for _, name := range account.Models {
				if name == model.Name {
					return account.Provider
				}
			}
		}
		return ""
	}
	if got := read(); got != "first-vendor" {
		t.Fatal("alias not resolved", got)
	}
	// A report-local resolver may reuse one snapshot, but a later report must
	// observe replacement and removal without a stale positive/negative cache.
	model.Provider = "second-vendor"
	if e := cat.SaveModel(model); e != nil {
		t.Fatal(e)
	}
	if got := read(); got != "second-vendor" {
		t.Fatal("catalog edit hidden", got)
	}
	if e := cat.RemoveModel(model.Name); e != nil {
		t.Fatal(e)
	}
	if got := read(); got != "" {
		t.Fatal("removed model remained resolved", got)
	}
	resolve, _ := reportModelResolver(g)
	if _, ok := resolve("report-fixture-never-declared"); ok {
		t.Fatal("unknown model invented")
	}
}
