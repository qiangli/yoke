package seedfit

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestDeclaredScales(t *testing.T) {
	for _, c := range []struct {
		metric, score string
		want          float64
		reason        string
	}{
		{"Elo [elo anchor 1500]", "1500", 0.5, ""},
		{"Elo [elo anchor 1500]", "1900", 1 / (1 + math.Pow(10, -1)), ""},
		{"final balance USD [range 0..20000]", "$16,843.41 ±$972", 16843.41 / 20000, ""},
		{"avg score [range 0..2]", "1.5", 0.75, ""},
		{"index [range -100..100]", "-11.2", 0.444, ""},
		{"index [range 0..100]", "120", 0, SkipOutOfRange},
		{"index [range 0..100]", "n/a", 0, SkipUnparseable},
		{"Elo", "1500", 0, SkipRatingScale}, // undeclared: unchanged behaviour
	} {
		got, reason := ParseScore(c.metric, c.score)
		if reason != c.reason || (reason == "" && math.Abs(got-c.want) > 1e-3) {
			t.Errorf("%q %q: got %v %q, want %v %q", c.metric, c.score, got, reason, c.want, c.reason)
		}
	}
}

func TestLineageInheritsOnlyMissingCells(t *testing.T) {
	rows := []Row{
		{Model: "x-1", Benchmark: "b1", Score: "50"},
		{Model: "x-1", Benchmark: "b2", Score: "40"},
		{Model: "x-2", Benchmark: "b1", Score: "60"},
		{Model: "y-1", Benchmark: "b3", Score: "70"},
	}
	out, log := ApplyLineage(rows, []LineageChain{{Vendor: "v", Line: "x", Models: []string{"x-1", "x-2"}}})
	if len(log) != 1 || log[0].Model != "x-2" || log[0].Benchmark != "b2" || log[0].From != "x-1" {
		t.Fatalf("want only x-2/b2 inherited from x-1, got %+v", log)
	}
	for _, r := range out {
		if r.Model == "x-2" && r.Benchmark == "b1" && r.Score != "60" {
			t.Errorf("real row overwritten")
		}
		if r.Model == "y-1" && r.Inherited != "" {
			t.Errorf("model outside the chain inherited")
		}
	}
}

func TestPerAreaProfile(t *testing.T) {
	m := writeAreaSim(t, "browser-use")
	c := writeConfig(t, "schema: bashy-seedfit-config-v1\nareas:\n  coding-debug-test: {weight: 0.6, hands_on: true}\n  browser-use: {weight: 0.4}\nanchors: [model-e, model-h]\n")
	code, out, errs := runMain("--matrix", m, "--config", c, "--per-area")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"## Ranking", "## Area matrix", "## Area fits", "| model-f | 0.59 ± 0.02 | 0.59 ± 0.03 | 5: model-e, model-d, model-c, model-b, model-a |", "| candidate |", "| model-d | -0.19 ± 0.02 | -0.19 ± 0.02 | 3: model-c, model-b, model-a |", "| below line |", "| model-a | -1.56 ± 0.02 | -1.56 ± 0.03 | 0:  |"} {
		if !strings.Contains(out, want) {
			t.Errorf("profile missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "| model-h |") > strings.Index(out, "| model-a |") {
		t.Errorf("ranking not by role desc")
	}
	if c2, _, _ := runMain("--matrix", m, "--per-area"); c2 != 2 {
		t.Errorf("--per-area without --config: exit %d want 2", c2)
	}
	_, o2, _ := runMain("--matrix", m, "--config", c, "--per-area")
	if o2 != out {
		t.Errorf("profile not deterministic")
	}
}

func TestProfileFloorsMissingArea(t *testing.T) {
	m := writeAreaSim(t, "browser-use")
	// Drop model-c from browser-use: it must be floored and flagged.
	rows, _ := readRowsForTest(t, m)
	var kept []Row
	for _, r := range rows {
		if !(r.Model == "model-c" && r.Area == "browser-use") {
			kept = append(kept, r)
		}
	}
	cfg := &Config{Schema: ConfigSchema, Areas: map[string]AreaConfig{"coding-debug-test": {Weight: .5, HandsOn: true}, "browser-use": {Weight: .5}}, Anchors: []string{"model-h"}}
	p := BuildProfile(FitPerArea(kept, cfg), cfg, nil)
	for _, r := range p.Rows {
		if r.Model == "model-c" {
			s := r.Areas["browser-use"]
			if !s.Floor || len(r.MustMeasure) != 1 || r.MustMeasure[0] != "browser-use" {
				t.Fatalf("model-c browser-use not floored/flagged: %+v %v", s, r.MustMeasure)
			}
			return
		}
	}
	t.Fatal("model-c missing from profile")
}

func readRowsForTest(t *testing.T, path string) ([]Row, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ReadMatrix(f)
}

func TestFloorMissingCells(t *testing.T) {
	obs := []Obs{
		{Model: "a", Item: "b1", Y: 0.60}, {Model: "b", Item: "b1", Y: 0.40},
		{Model: "a", Item: "b2", Y: 0.03}, {Model: "c", Item: "b2", Y: 0.50},
	}
	count := map[string]int{}
	out := floorMissing(obs, []string{"a", "b", "c", "z"}, map[string]string{"a": "v", "b": "v", "c": "v"}, 0.05, 0.5, count)
	got := map[[2]string]Obs{}
	for _, o := range out {
		got[[2]string{o.Model, o.Item}] = o
	}
	if o := got[[2]string{"c", "b1"}]; math.Abs(o.Y-0.35) > 1e-9 || o.Source != ReferenceSource || o.Harness != ReferenceHarness {
		t.Errorf("c/b1 floor = %+v, want 0.35 at the reference levels", o)
	}
	if o := got[[2]string{"b", "b2"}]; o.Y != ScoreClampLo {
		t.Errorf("floor must stay above zero: got %v", o.Y)
	}
	if _, ok := got[[2]string{"z", "b1"}]; ok {
		t.Errorf("a candidate with no vendor (absent from the matrix) must not be floored")
	}
	if count["c"] != 1 || count["b"] != 1 || count["a"] != 0 {
		t.Errorf("floored counts %v", count)
	}
}

func TestFloorSkipsSparseBoards(t *testing.T) {
	obs := []Obs{{Model: "a", Item: "sparse", Y: 0.9}, {Model: "b", Item: "sparse", Y: 0.8}}
	count := map[string]int{}
	cands := []string{"a", "b", "c", "d", "e"}
	v := map[string]string{"a": "v", "b": "v", "c": "v", "d": "v", "e": "v"}
	if out := floorMissing(obs, cands, v, 0.05, 0.5, count); len(out) != 2 {
		t.Errorf("2 of 5 candidates < 0.5 coverage: nothing may be floored, got %d obs", len(out))
	}
	if out := floorMissing(obs, cands, v, 0.05, 0.4, count); len(out) != 5 {
		t.Errorf("2 of 5 = 0.4 coverage: c, d, e must be floored, got %d obs", len(out))
	}
}
