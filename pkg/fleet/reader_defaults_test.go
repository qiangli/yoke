package fleet

import (
	"reflect"
	"testing"
)

func TestMatchCommandDefaultsSpecificityAndSources(t *testing.T) {
	prefs, src, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
		EnvJSON: `{"defaults":{"*":{"no_input":true,"depth":3},"graph *":{"depth":1},"rm":{"dry_run":true}}}`,
		Agent:   &Agent{Prefs: ReaderPrefs{Defaults: map[string]map[string]any{"graph *": {"depth": 9, "fields": "a"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := MatchCommandDefaults(prefs, src, "graph  build")
	want := map[string]any{"no_input": true, "depth": float64(1), "fields": "a"}
	if !reflect.DeepEqual(got.Options, want) {
		t.Fatalf("options = %v, want %v", got.Options, want)
	}
	if got.Sources["depth"] != SourceEnv || got.Sources["fields"] != SourceAgent || got.Pattern["depth"] != "graph *" {
		t.Fatalf("sources = %v patterns = %v", got.Sources, got.Pattern)
	}
	if rm := MatchCommandDefaults(prefs, src, "rm"); rm.Options["dry_run"] != true || rm.Options["depth"] != float64(3) {
		t.Fatalf("rm = %v", rm.Options)
	}
	if ls := MatchCommandDefaults(prefs, src, "ls"); len(ls.Options) != 2 {
		t.Fatalf("ls = %v", ls.Options)
	}
	if !reflect.DeepEqual(got.Flags(), []string{"--depth=1", "--fields=a", "--no-input"}) {
		t.Fatalf("flags = %v", got.Flags())
	}
}

func TestPlanSinkProfileHelpDialectAndCanonicalEscape(t *testing.T) {
	prefs, src, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
		EnvJSON: `{"view":"yaml","help":"script","dialect":"pwsh","budget_bytes":16000}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := PlanSink(prefs, src, false)
	if p.View != "yaml" || p.Help != "script" || p.Dialect != "pwsh" || p.HelpSource != SourceEnv || p.Canonical {
		t.Fatalf("plan = %+v", p)
	}
	c := PlanSink(prefs, src, true)
	if !c.Canonical || c.View != ViewCanonical || c.ViewSource != SourceExplicit || c.Help != "script" {
		t.Fatalf("canonical plan = %+v", c)
	}
	if want := "view=yaml(env) help=script(env) dialect=pwsh(env) budget_bytes=16000(env)"; p.Profile() != want {
		t.Fatalf("profile = %q", p.Profile())
	}
}
