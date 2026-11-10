package llmprice

import (
	"math"
	"testing"
)

func closeEnough(got, want float64) bool { return math.Abs(got-want) < 1e-12 }

// THE DOOR NEVER FALLS BACK TO ANOTHER MODEL'S PRICE.
//
// The seed table's fallback was Claude Sonnet's rate ($3/$15 per M) with a
// Known flag nobody read, so GLM-5.2 — one of the cheapest models in the
// fleet — was billed at 5x its API price and a cost dashboard showed the
// cheapest model as the dearest. An unknown model must be unknown.
func TestUnknownModelIsUnknownNotFallback(t *testing.T) {
	p := Lookup("some-model-nobody-has-heard-of-v9")
	if p.Known {
		t.Fatal("a model absent from the table reported Known=true")
	}
	if p.InputPerM != 0 || p.OutputPerM != 0 || p.CacheWritePerM != 0 || p.CacheReadPerM != 0 {
		t.Errorf("unknown model carries rates: %+v", p)
	}
	if cost, ok := Cost("some-model-nobody-has-heard-of-v9", Usage{Input: 1_000_000}); ok || cost != 0 {
		t.Errorf("unknown model priced: cost=%v ok=%v", cost, ok)
	}
}

// The specific lie, pinned: GLM must not be billed at Claude rates.
func TestGLMIsNotBilledAtClaudeRates(t *testing.T) {
	glm := Lookup("glm-5.2")
	claude := Lookup("claude-sonnet-4")
	if !glm.Known || !claude.Known {
		t.Fatal("glm-5.2 or claude-sonnet-4 missing from the table")
	}
	if glm.InputPerM >= claude.InputPerM {
		t.Errorf("glm input $%.2f/M at or above claude's $%.2f/M", glm.InputPerM, claude.InputPerM)
	}
	const in, out = 100_000, 10_000
	real, _ := Cost("glm-5.2", Usage{Input: in, Output: out})
	lie := (float64(in)*claude.InputPerM + float64(out)*claude.OutputPerM) / 1e6
	if real >= lie {
		t.Errorf("100k/10k turn: glm $%.4f, claude rate would say $%.4f", real, lie)
	}
}

// Every model the fleet can actually route to must be priced. A missing row
// is not a gap in a table; it is a wrong number in a metering report.
func TestFleetModelsHaveRealPrices(t *testing.T) {
	for _, model := range []string{
		"glm-5.3", "glm-5.2", "glm-4.6", "glm-5", "zai.glm-5",
		"kimi-k2.7-code", "moonshot/kimi-k2.7-code",
		"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat",
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5",
		"gpt-4.1", "o3",
		"gemini-2.5-pro", "gemini-2.5-flash",
		"qwen-max", "grok-3",
	} {
		if !Default.Known(model) {
			t.Errorf("%s has no declared price — the door will record it as unpriced", model)
		}
	}
}

// Local models (ollama) are deliberately unpriced: a host's own weights have
// no list price, and marking them $0 would erase the difference between
// "free" and "we do not know".
func TestLocalModelsAreUnpriced(t *testing.T) {
	for _, model := range []string{
		"llama3.1:8b", "qwen2.5-coder:latest", "gpt-oss-120b", "muse-spark-1.3", "muse-spark1.3",
	} {
		if p := Lookup(model); p.Known {
			t.Errorf("%s carries a price; local engine models must be unpriced, got %+v", model, p)
		}
	}
}

func TestCostSumsEveryBillingItem(t *testing.T) {
	// 1M input + 1M output + 1M cache write + 1M cache read on
	// claude-sonnet-4 = 3 + 15 + 3.75 + 0.30 = $22.05
	cost, ok := Cost("claude-sonnet-4", Usage{
		Input: 1_000_000, Output: 1_000_000, CacheWrite: 1_000_000, CacheRead: 1_000_000,
	})
	if !ok || !closeEnough(cost, 22.05) {
		t.Fatalf("cost = %v (ok=%v); want 22.05", cost, ok)
	}
}

func TestCacheItemsArePricedSeparately(t *testing.T) {
	cold, _ := Cost("claude-sonnet-4", Usage{Input: 1_000_000})
	warm, _ := Cost("claude-sonnet-4", Usage{CacheRead: 1_000_000})
	written, _ := Cost("claude-sonnet-4", Usage{CacheWrite: 1_000_000})
	if !closeEnough(cold, 3.0) || !closeEnough(warm, 0.30) || !closeEnough(written, 3.75) {
		t.Errorf("cold=%v warm=%v write=%v; want 3.0, 0.30, 3.75", cold, warm, written)
	}
}

func TestZeroTokensCostZeroAndStaysKnown(t *testing.T) {
	cost, ok := Cost("claude-opus-4", Usage{})
	if !ok || cost != 0 {
		t.Errorf("zero usage: cost=%v ok=%v; want 0, true (priced at zero, not unknown)", cost, ok)
	}
}

func TestADeclaredZeroRateIsAPrice(t *testing.T) {
	// OpenAI has no cache-write charge: the declared zero must not make the
	// model read as unpriced.
	p := Lookup("gpt-4o")
	if !p.Known || p.CacheWritePerM != 0 {
		t.Fatalf("gpt-4o cache write = %+v; want a declared 0 with Known=true", p)
	}
	if _, ok := Cost("gpt-4o", Usage{CacheWrite: 1_000_000}); !ok {
		t.Error("gpt-4o with cache-write tokens only reported unknown")
	}
}

func TestUsageTokensCountsEveryBucket(t *testing.T) {
	u := Usage{Input: 10, Output: 5, CacheWrite: 100, CacheRead: 1000}
	if u.Tokens() != 1115 {
		t.Errorf("Tokens() = %d; want 1115", u.Tokens())
	}
}

// A bare family alias would price every future model of that family at
// today's number. Prefix matching is how the fallback lie worked.
func TestNoFamilyAliasCapturesAModelItDoesNotName(t *testing.T) {
	for _, model := range []string{"glm-9.9", "kimi-k9", "claude-opus-9", "grok-9", "qwen-99"} {
		if p := Lookup(model); p.Known {
			t.Errorf("%s matched a family alias and inherited a price it was not declared for", model)
		}
	}
}

// ---- Bedrock id normalization ---------------------------------------------

func TestNormalizeBedrockModelIDs(t *testing.T) {
	tests := []struct{ in, want string }{
		// Cross-region inference profiles: region + vendor prefix.
		{"us.anthropic.claude-opus-4-8", "claude-opus-4-8"},
		{"eu.anthropic.claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"apac.anthropic.claude-haiku-4-5", "claude-haiku-4-5"},
		{"global.anthropic.claude-opus-4-8", "claude-opus-4-8"},
		{"us-gov-west-1.anthropic.claude-sonnet-4-6", "claude-sonnet-4-6"},
		// A single-region spelling.
		{"us-east-1.anthropic.claude-opus-4-8", "claude-opus-4-8"},
		// Third-party vendor namespaces on Bedrock.
		{"zai.glm-5", "glm-5"},
		{"openai.gpt-6-sol", "gpt-6-sol"},
		{"amazon.nova-pro-v1", "nova-pro-v1"},
		{"meta.llama3-3-70b-instruct", "llama3-3-70b-instruct"},
		{"deepseek.deepseek-v4-pro", "deepseek-v4-pro"},
		// A tool's own provider-prefixed spelling of the same route.
		{"amazon-bedrock/zai.glm-5", "glm-5"},
		{"openai-compat/deepseek-v4-pro", "deepseek-v4-pro"},
		{"zai-coding-plan/glm-5.3", "glm-5.3"},
		{"moonshot/kimi-k2.7-code", "kimi-k2.7-code"},
		{"qwen/qwen2.5-coder", "qwen2.5-coder"},
		// Case and whitespace are not part of an id's identity.
		{"US.Anthropic.Claude-Opus-4-8", "claude-opus-4-8"},
		{"  zai.glm-5  ", "glm-5"},
		// An Ollama tag is not part of the model.
		{"llama3.1:8b", "llama3.1"},
		{"qwen2.5-coder:latest", "qwen2.5-coder"},
		// Bare names are untouched, including names that merely start with a
		// vendor or region word.
		{"claude-opus-4-8", "claude-opus-4-8"},
		{"glm-5.3", "glm-5.3"},
		{"qwen2.5-coder", "qwen2.5-coder"},
		{"us-east-1", "us-east-1"},
		{"zai", "zai"},
		{"", ""},
	}
	for _, test := range tests {
		if got := NormalizeModel(test.in); got != test.want {
			t.Errorf("NormalizeModel(%q) = %q; want %q", test.in, got, test.want)
		}
	}
}

// A version number is not a region: the dot rule must not eat "glm-5.3".
func TestNormalizationDoesNotEatVersionNumbers(t *testing.T) {
	if got := NormalizeModel("glm-5.3"); got != "glm-5.3" {
		t.Errorf("glm-5.3 -> %q", got)
	}
	if got := NormalizeModel("gpt-4.1"); got != "gpt-4.1" {
		t.Errorf("gpt-4.1 -> %q", got)
	}
}

// Every spelling of one model's weights prices identically.
func TestEverySpellingOfOneModelPricesTheSame(t *testing.T) {
	for _, group := range [][]string{
		{"claude-opus-4-8", "us.anthropic.claude-opus-4-8", "eu.anthropic.claude-opus-4-8", "us-east-1.anthropic.claude-opus-4-8"},
		{"glm-5", "zai.glm-5", "amazon-bedrock/zai.glm-5"},
		{"deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
	} {
		first := Lookup(group[0])
		if !first.Known {
			t.Fatalf("%s is not priced; it anchors its group", group[0])
		}
		for _, spelling := range group[1:] {
			if p := Lookup(spelling); p != first {
				t.Errorf("%q prices as %+v but %q prices as %+v", group[0], first, spelling, p)
			}
		}
	}
}

// The Bedrock rows this repository's fleet seeds actually pin.
func TestFleetBedrockSeedsArePriced(t *testing.T) {
	for _, id := range []string{
		"us.anthropic.claude-opus-4-8", // models/bedrock-opus4.8.yaml
		"zai.glm-5",                    // models/bedrock-glm-5.yaml
	} {
		if !Default.Known(id) {
			t.Errorf("Bedrock id %s (pinned by a fleet seed) has no price", id)
		}
	}
}

// A Table with no rows answers unknown for everything — the honest answer,
// never a fallback price.
func TestEmptyTableAnswersUnknown(t *testing.T) {
	empty := &Table{}
	if p := empty.Lookup("claude-sonnet-4"); p.Known {
		t.Errorf("empty table priced claude-sonnet-4: %+v", p)
	}
	if _, ok := empty.Cost("claude-sonnet-4", Usage{Input: 1}); ok {
		t.Error("empty table reported a cost")
	}
}

// An operator overlay adds prices without touching the build, and cannot
// make an unknown model priced by accident: it names the models it prices.
func TestRegisterAddsRows(t *testing.T) {
	t.Cleanup(func() {
		defaultMu.Lock()
		delete(defaultModels, "acme-9")
		defaultMu.Unlock()
	})
	Register(map[string]Pricing{"acme-9": {InputPerM: 1, OutputPerM: 2, Known: true}})
	if p := Lookup("acme-9"); !p.Known || p.InputPerM != 1 {
		t.Fatalf("registered model not found: %+v", p)
	}
	// And its prefixed spellings price the same row.
	if p := Lookup("us.acme.acme-9"); !p.Known || p.InputPerM != 1 {
		t.Errorf("registered model's Bedrock spelling not normalized to it: %+v", p)
	}
}
