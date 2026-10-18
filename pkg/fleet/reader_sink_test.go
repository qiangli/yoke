package fleet

import (
	"reflect"
	"testing"
)

func TestSinkEscape(t *testing.T) {
	for _, c := range [][]string{{"--full"}, {"--view=canonical"}, {"--view", "canonical"}} {
		rest, canon := SinkEscape(append([]string{"a"}, c...))
		if !canon || !reflect.DeepEqual(rest, []string{"a"}) {
			t.Errorf("%v: rest=%v canon=%v", c, rest, canon)
		}
	}
	for _, c := range [][]string{{"--view", "json", "x"}, {"--view=Canonical"}, {"--view"}, {"--view="}, {"--full=1"}} {
		rest, canon := SinkEscape(c)
		if canon || !reflect.DeepEqual(rest, c) {
			t.Errorf("%v must be retained without escape: %v %v", c, rest, canon)
		}
	}
}

func TestPlanSink(t *testing.T) {
	p := PlanSink(ReaderPrefs{BudgetBytes: 100}, ReaderPrefsSources{BudgetBytes: "agent"}, false)
	if p.BudgetBytes != 100 || p.Canonical || p.Source != "agent" {
		t.Fatalf("%+v", p)
	}
	if !PlanSink(ReaderPrefs{}, ReaderPrefsSources{}, true).Canonical {
		t.Fatal("escape")
	}
	if p := PlanSink(ReaderPrefs{View: ViewCanonical, BudgetBytes: -5}, ReaderPrefsSources{}, false); !p.Canonical || p.BudgetBytes != 0 || p.Source != SourceDefault {
		t.Fatalf("%+v", p)
	}
}
