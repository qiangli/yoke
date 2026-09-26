package resolve

import (
	"strings"
	"testing"
)

func TestIsAutoModelName(t *testing.T) {
	for in, want := range map[string]bool{
		"auto":          true,
		"Auto":          true,
		"AUTO":          true,
		"smart":         true,
		"":              false,
		"automatic":     false,
		"qwen-coder:7b": false,
	} {
		if got := IsAutoModelName(in); got != want {
			t.Errorf("IsAutoModelName(%q)=%v, want %v", in, got, want)
		}
	}
}

func TestExtractFirstUserPrompt_ChatString(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"}]}`)
	if got := ExtractFirstUserPrompt(body); got != "hello world" {
		t.Errorf("got %q, want hello world", got)
	}
}

func TestExtractFirstUserPrompt_ChatPartsArray(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"write a function"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`)
	got := ExtractFirstUserPrompt(body)
	if !strings.Contains(got, "write a function") {
		t.Errorf("got %q, want substring 'write a function'", got)
	}
}

func TestExtractFirstUserPrompt_Embeddings(t *testing.T) {
	body := []byte(`{"input":"text to embed"}`)
	if got := ExtractFirstUserPrompt(body); got != "text to embed" {
		t.Errorf("got %q, want text to embed", got)
	}
	// Array input.
	body2 := []byte(`{"input":["first","second"]}`)
	if got := ExtractFirstUserPrompt(body2); got != "first" {
		t.Errorf("got %q, want first", got)
	}
}

func TestHasImagePart(t *testing.T) {
	yes := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`)
	if !hasImagePart(yes) {
		t.Errorf("image body not detected")
	}
	no := []byte(`{"messages":[{"role":"user","content":"plain"}]}`)
	if hasImagePart(no) {
		t.Errorf("plain body flagged as image")
	}
}

func TestPromptHeuristic_CodeMarker(t *testing.T) {
	_, doms := promptHeuristic("```python\nprint('hi')\n```", false)
	want := DomainCoding
	if !contains(doms, want) {
		t.Errorf("code marker missed; doms=%v want %s", doms, want)
	}
}

func TestPromptHeuristic_Vision(t *testing.T) {
	_, doms := promptHeuristic("what's in this image?", true)
	if !contains(doms, DomainVision) {
		t.Errorf("vision flag missed; doms=%v", doms)
	}
}

func TestPromptHeuristic_Reasoning(t *testing.T) {
	_, doms := promptHeuristic("let's think step by step about this", false)
	if !contains(doms, DomainReasoning) {
		t.Errorf("reasoning flag missed; doms=%v", doms)
	}
}

func TestPromptHeuristic_DefaultGeneral(t *testing.T) {
	tier, doms := promptHeuristic("hello, how are you?", false)
	if tier != TierL2 {
		t.Errorf("tier=%d, want L2 default", tier)
	}
	if len(doms) != 1 || doms[0] != DomainGeneral {
		t.Errorf("doms=%v, want [general]", doms)
	}
}

func TestPromptHeuristic_LongPromptUpgradesTier(t *testing.T) {
	long := strings.Repeat("blah ", 2000) // ~10k chars
	tier, _ := promptHeuristic(long, false)
	if tier != TierL3 {
		t.Errorf("long prompt tier=%d, want L3", tier)
	}
}

// fakeHistory is the in-test stand-in for the scheduler's recent-
// resolution buffer, which the chain reaches through the
// PromptHistory seam.
type fakeHistory map[string]PromptResolution

func (f fakeHistory) Lookup(key string) (PromptResolution, bool) {
	rec, ok := f[key]
	return rec, ok
}

func TestClassifyFromPrompt_HistoryHit(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"recurring familiar prompt"}]}`)
	key := PromptPrefixKey(ExtractFirstUserPrompt(body))
	hist := fakeHistory{key: {Tier: TierL2, Domains: []string{DomainCoding}, Success: true}}

	tier, doms, stage := ClassifyFromPrompt(body, hist)
	if stage != "history_nn" {
		t.Errorf("stage=%q, want history_nn", stage)
	}
	if tier != TierL2 || !contains(doms, DomainCoding) {
		t.Errorf("got (%d, %v), want history-recorded (%d, %v)", tier, doms, TierL2, []string{DomainCoding})
	}
}

func TestClassifyFromPrompt_FallsThroughToHeuristic(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"unique prompt no history"}]}`)
	tier, doms, stage := ClassifyFromPrompt(body, fakeHistory{})
	if stage != "heuristic" {
		t.Errorf("stage=%q, want heuristic", stage)
	}
	if tier == 0 || len(doms) == 0 {
		t.Errorf("heuristic must produce something; got (%d, %v)", tier, doms)
	}
}

// A nil PromptHistory just skips the History-NN stage.
func TestClassifyFromPrompt_NilHistorySkipsStage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	tier, doms, stage := ClassifyFromPrompt(body, nil)
	if stage != "heuristic" {
		t.Errorf("stage=%q, want heuristic", stage)
	}
	if tier != TierL2 || !contains(doms, DomainGeneral) {
		t.Errorf("got (%d, %v), want (L2, [general])", tier, doms)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
