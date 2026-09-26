package cligw

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestSelectorGrammarAndCandidates(t *testing.T) {
	cat := testFleet(t)
	cases := []struct {
		selector string
		want     []string
	}{
		{"L4", []string{"gamma", "x-cascade"}},
		{"L4+", []string{"gamma", "x-cascade", "omega"}},
		{"swift", []string{"alpha", "beta", "x-cascade"}},
		{"first", []string{"alpha"}},
		{"beta-tool:strong", []string{"gamma"}},
	}
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			sel, err := cat.ParseModelSelector(tc.selector)
			if err != nil {
				t.Fatal(err)
			}
			got := agentNames(cat.Candidates(context.Background(), sel, Filter{}))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("Candidates(%q) = %v, want %v", tc.selector, got, tc.want)
			}
		})
	}
}

func TestAutoClassifiesThenUsesExactBand(t *testing.T) {
	cat := testFleet(t)
	sel, err := cat.ParseModelSelector("auto")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"summarize this"}]}`)
	got := agentNames(cat.CandidatesFromPrompt(context.Background(), sel, Filter{}, body, nil))
	want := []string{"alpha", "beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("auto candidates = %v, want exact L2 %v", got, want)
	}
}

func TestFilterNarrowingAndConfigDefault(t *testing.T) {
	cat := testFleet(t)
	sel, err := cat.ParseModelSelector("L2+")
	if err != nil {
		t.Fatal(err)
	}
	filter, err := ParseFilterWithDefault("provider=openai,tool=beta-tool", Filter{Kind: fleet.ModelKindSubscription, Tool: "alpha-tool"})
	if err != nil {
		t.Fatal(err)
	}
	// The request overrides tool but retains the config's subscription kind.
	if got := cat.Candidates(context.Background(), sel, filter); len(got) != 0 {
		t.Fatalf("intersection should be empty, got %+v", got)
	}
	filter, err = ParseFilter("kind=api,provider=openai,tool=beta-tool,band_source=measured")
	if err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cat.Candidates(context.Background(), sel, filter)); strings.Join(got, ",") != "gamma" {
		t.Fatalf("filtered candidates = %v, want gamma", got)
	}
	if _, err := ParseFilter("account=paid"); err == nil {
		t.Fatal("unknown filter key accepted")
	}
}

func TestUnknownSelectorSuggestsNearestRegistryNames(t *testing.T) {
	cat := testFleet(t)
	_, err := cat.ParseModelSelector("strnog")
	var unknown *UnknownSelectorError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %T %v, want UnknownSelectorError", err, err)
	}
	if len(unknown.Suggestions) == 0 || unknown.Suggestions[0] != "strong" {
		t.Fatalf("suggestions = %v, want strong first", unknown.Suggestions)
	}
}

func TestInvalidBandIsUnknownRatherThanSilentlyClamped(t *testing.T) {
	cat := testFleet(t)
	if _, err := cat.ParseModelSelector("L6"); err == nil {
		t.Fatal("L6 accepted")
	}
}

func agentNames(in []Agent) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Name)
	}
	return out
}
