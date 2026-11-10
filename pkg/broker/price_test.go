package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmprice"
)

// A priced model through the fleet backend gets cost_usd and no unpriced flag.
// No Pricer is set: the door's default is the llmprice table, which is what a
// production door runs with. The Bedrock route is what this sprint's UAT rides
// on, so its spelling is pinned below: us.anthropic.claude-opus-4-8 prices as
// claude-opus-4-8.
func TestFinishWritesCostUSDForPricedModel(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do("POST", "/v1/chat/completions", chat("claude-opus-4-8"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if rec.Unpriced {
		t.Fatalf("priced model recorded unpriced: %+v", rec)
	}
	if rec.CostUSD <= 0 {
		t.Fatalf("no cost_usd on a priced record: %+v", rec)
	}
	// prompt_tokens 2 + completion_tokens 1 at $15/$75 per M = $105/M ≈ $0.000105
	want := (2*15.0 + 1*75.0) / 1e6
	if diff := rec.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost_usd = %.9f; want %.9f (2 in + 1 out at $15/$75 per M)", rec.CostUSD, want)
	}
	// The wire carries the field by its JSON name.
	if _, err := json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
}

func TestPriceSeparatesCacheWrites(t *testing.T) {
	pricing := llmprice.Lookup("claude-sonnet-4")
	if !pricing.Known || pricing.CacheWritePerM == pricing.InputPerM {
		t.Fatal("test requires distinct Claude input and cache-write rates")
	}
	ri := &reqInfo{rec: Record{Backend: BackendCLI, Model: "claude-sonnet-4", PromptTok: 15, CachedTok: 7, CacheWriteTok: 5, OutputTok: 2}}
	h := newHarness(t, nil)
	h.b.price(ri)
	want := (3*pricing.InputPerM + 7*pricing.CacheReadPerM + 5*pricing.CacheWritePerM + 2*pricing.OutputPerM) / 1e6
	if ri.rec.CostUSD != want || ri.rec.PromptTok != 15 {
		t.Errorf("record = %+v; want whole prompt 15 and cost %.9f", ri.rec, want)
	}
}

// A model the table has never heard of is UNPRICED, not priced at another
// model's rate. This is the rule the story states outright: the door never
// falls back to another model's price.
func TestFinishMarksUnknownModelUnpriced(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do("POST", "/v1/chat/completions", chat("mystery-model-v9"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if !rec.Unpriced {
		t.Errorf("unknown model not marked unpriced: %+v", rec)
	}
	if rec.CostUSD != 0 {
		t.Errorf("unknown model carries cost_usd = %v; a fallback price is a wrong price", rec.CostUSD)
	}
}

// A local engine model (ollama) is unpriced: the host serves its own weights,
// and no vendor bills it — even when the name would match a table row.
func TestFinishMarksLocalModelsUnpriced(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do("POST", "/v1/chat/completions", chat("llama3.2:3b"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if !rec.Unpriced {
		t.Errorf("local engine model not marked unpriced: %+v", rec)
	}
	if rec.CostUSD != 0 {
		t.Errorf("local engine model carries cost_usd = %v", rec.CostUSD)
	}
	if rec.Backend != BackendLocal {
		t.Errorf("backend = %q; want %q for the test to mean what it says", rec.Backend, BackendLocal)
	}
}

// A fleet record's Model is the agent alias the client asked for; the price
// comes from the model the vendor actually billed, A1's UpstreamModel. A
// sticky CLI record with an unpriced alias but a priced upstream model gets a
// cost_usd, and an unknown upstream id stays unpriced: the door does not fall
// back to pricing the alias, in either direction.
func TestPriceUsesUpstreamModelOverAgentAlias(t *testing.T) {
	priced := llmprice.Lookup("claude-opus-4-8")
	if !priced.Known {
		t.Fatal("claude-opus-4-8 must be priced for this test to mean anything")
	}
	cases := []struct {
		name     string
		rec      Record
		wantCost bool
	}{
		{
			// A sticky fleet turn: Model is the agent, UpstreamModel is what
			// the vendor billed. Priced via the upstream id.
			name:     "agent alias with priced upstream",
			rec:      Record{Backend: BackendCLI, Model: "agent-alias", UpstreamModel: "us.anthropic.claude-opus-4-8", PromptTok: 2, OutputTok: 1},
			wantCost: true,
		},
		{
			// No upstream id (a local-ish route, or a backend that reports
			// none): Model is what there is, and it prices as itself.
			name:     "model only",
			rec:      Record{Backend: BackendCLI, Model: "claude-opus-4-8", PromptTok: 2, OutputTok: 1},
			wantCost: true,
		},
		{
			// The upstream id is unknown: unpriced, NOT the alias's price
			// (and the alias is unpriced anyway here, but the rule is the
			// same when the alias would price).
			name:     "unknown upstream stays unknown",
			rec:      Record{Backend: BackendCLI, Model: "claude-opus-4-8", UpstreamModel: "mystery-upstream-v9", PromptTok: 2, OutputTok: 1},
			wantCost: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri := &reqInfo{rec: tc.rec}
			h := newHarness(t, nil)
			h.b.price(ri)
			got := ri.rec
			if tc.wantCost {
				if got.Unpriced || got.CostUSD <= 0 {
					t.Errorf("want cost_usd, got cost=%v unpriced=%v", got.CostUSD, got.Unpriced)
				}
				want := (2*priced.InputPerM + 1*priced.OutputPerM) / 1e6
				if got.CostUSD != want {
					t.Errorf("cost_usd = %.9f; want %.9f", got.CostUSD, want)
				}
			} else if !got.Unpriced || got.CostUSD != 0 {
				t.Errorf("want unpriced, got cost=%v unpriced=%v", got.CostUSD, got.Unpriced)
			}
		})
	}
}

// Cached tokens are billed at the cache-read rate, not the input rate, and
// they are not billed twice: the pricer receives the uncached prompt as Input
// and the cached part as CacheRead. PromptTok stays the whole prompt on the
// record (what a usage report shows), so the split happens here.
func TestPriceSplitsCachedFromUncachedInput(t *testing.T) {
	p := llmprice.Lookup("claude-opus-4-8")
	if !p.Known {
		t.Fatal("claude-opus-4-8 must be priced for this test to mean anything")
	}
	var gotModel string
	var gotUsage llmprice.Usage
	h := newHarness(t, func(o *Options) {
		o.Pricer = func(model string, u llmprice.Usage) (float64, bool) {
			gotModel, gotUsage = model, u
			return llmprice.Default.Cost(model, u)
		}
	})
	ri := &reqInfo{rec: Record{Backend: BackendCLI, Model: "claude-opus-4-8",
		PromptTok: 1000, CachedTok: 400, OutputTok: 10}}
	h.b.price(ri)
	if gotModel != "claude-opus-4-8" {
		t.Errorf("pricer saw model %q; want claude-opus-4-8", gotModel)
	}
	if gotUsage.Input != 600 || gotUsage.CacheRead != 400 {
		t.Errorf("usage = input %d cache_read %d; want 600 and 400 (prompt 1000, cached 400)",
			gotUsage.Input, gotUsage.CacheRead)
	}
	if gotUsage.Output != 10 {
		t.Errorf("usage output = %d; want 10", gotUsage.Output)
	}
	// And the cost reflects the split: 600 in + 400 cache-read + 10 out.
	want := (600*p.InputPerM + 400*p.CacheReadPerM + 10*p.OutputPerM) / 1e6
	if ri.rec.CostUSD != want {
		t.Errorf("cost_usd = %.9f; want %.9f", ri.rec.CostUSD, want)
	}
	if ri.rec.Unpriced {
		t.Errorf("priced split recorded unpriced: %+v", ri.rec)
	}
	// The record keeps the whole prompt: the split is for the pricer only.
	if ri.rec.PromptTok != 1000 || ri.rec.CachedTok != 400 {
		t.Errorf("record tokens rewritten: prompt=%d cached=%d; want 1000 and 400",
			ri.rec.PromptTok, ri.rec.CachedTok)
	}
}

// A cached count larger than the prompt (a stream truncated before usage
// arrived, a provider counting its own way) clamps to a zero uncached input
// rather than handing the pricer a negative number.
func TestPriceClampsCachedLargerThanPrompt(t *testing.T) {
	var gotUsage llmprice.Usage
	h := newHarness(t, func(o *Options) {
		o.Pricer = func(model string, u llmprice.Usage) (float64, bool) {
			gotUsage = u
			return 1.0, true
		}
	})
	ri := &reqInfo{rec: Record{Backend: BackendCLI, Model: "claude-opus-4-8",
		PromptTok: 100, CachedTok: 300, OutputTok: 5}}
	h.b.price(ri)
	if gotUsage.Input != 0 {
		t.Errorf("input = %d; want 0 (clamped, not %d)", gotUsage.Input, 100-300)
	}
	if gotUsage.CacheRead != 300 {
		t.Errorf("cache_read = %d; want 300", gotUsage.CacheRead)
	}
}

// A refusal before any device work has no tokens to price: neither field is
// written, so a queue-full or memory refusal never reads as a free request.
func TestFinishWritesNoCostForTokenlessRefusal(t *testing.T) {
	h := newHarness(t, nil)
	// A model that is not on the engine and not a fleet agent is a 404, but
	// the CLI harness answers everything; use a bad body instead so the
	// record has no tokens.
	resp, _ := h.do("POST", "/v1/chat/completions", "not-a-json-object", nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected a refusal, got %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if rec.PromptTok != 0 || rec.OutputTok != 0 {
		t.Fatalf("expected a tokenless refusal, got %+v", rec)
	}
	if rec.CostUSD != 0 || rec.Unpriced {
		t.Errorf("tokenless refusal carries pricing fields: cost=%v unpriced=%v", rec.CostUSD, rec.Unpriced)
	}
}

// The Pricer hook is the seam an operator overlay or a catalog-driven price
// source plugs into, and a hook that says "unknown" is honored as unknown: the
// door must not substitute its own table's number behind the hook's back.
func TestCustomPricerUnknownIsHonored(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Pricer = func(string, llmprice.Usage) (float64, bool) { return 0, false }
	})
	resp, _ := h.do("POST", "/v1/chat/completions", chat("claude-opus-4-8"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if !rec.Unpriced || rec.CostUSD != 0 {
		t.Errorf("pricer said unknown but record says cost=%v unpriced=%v", rec.CostUSD, rec.Unpriced)
	}
}

// And a hook that prices a model the default table does not know is honored
// too — the overlay is the deployment's own price source.
func TestCustomPricerPriceIsHonored(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Pricer = func(model string, u llmprice.Usage) (float64, bool) {
			if model != "acme-9" {
				return 0, false
			}
			return float64(u.Input+u.Output) * 2 / 1e6, true
		}
	})
	resp, _ := h.do("POST", "/v1/chat/completions", chat("acme-9"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec := h.lastRecord(1)
	if rec.Unpriced || rec.CostUSD != (2+1)*2.0/1e6 {
		t.Errorf("custom price not honored: cost=%v unpriced=%v", rec.CostUSD, rec.Unpriced)
	}
}

// The door's record and the llmprice table agree on a Bedrock id's price: the
// normalization lives in the table, and the door passes the id through as any
// surface spelled it.
func TestBedrockIDPricesThroughTheDoor(t *testing.T) {
	for _, id := range []string{
		"us.anthropic.claude-opus-4-8",
		"eu.anthropic.claude-opus-4-8",
		"claude-opus-4-8",
	} {
		p := llmprice.Lookup(id)
		if !p.Known {
			t.Fatalf("%s is not priced by llmprice", id)
		}
	}
	if _, ok := llmprice.Cost("zai.glm-5", llmprice.Usage{Input: 1_000_000}); !ok {
		t.Error("zai.glm-5 (the Bedrock GLM seed's id) is unpriced")
	}
	if _, ok := llmprice.Cost("openai.gpt-6-sol", llmprice.Usage{Input: 1}); ok {
		t.Error("openai.gpt-6-sol is priced, but no per-million rate is documented for it; " +
			"the table must not invent one")
	}
}

// The minimal integration this story needs: one priced request and one local
// one, end to end through ServeHTTP, with the audit file on disk carrying both
// fields by their JSON names. That is what `bashy llm usage` will read.
func TestAuditFileCarriesCostFields(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(o *Options) { o.StateDir = dir })
	srv := httptest.NewServer(h.b)
	defer srv.Close()

	for _, model := range []string{"claude-opus-4-8", "llama3.2:3b"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			jsonBody(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", model, resp.StatusCode)
		}
	}

	// Read the audit file back and check the JSON names.
	var priced, unpriced int
	for _, line := range readLines(t, dir+"/audit.jsonl") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		switch {
		case raw["cost_usd"] != nil:
			priced++
			if raw["unpriced"] != nil {
				t.Errorf("record carries both cost_usd and unpriced: %v", line)
			}
		case raw["unpriced"] == true:
			unpriced++
			if raw["cost_usd"] != nil {
				t.Errorf("record carries both unpriced and cost_usd: %v", line)
			}
		}
	}
	if priced != 1 || unpriced != 1 {
		t.Errorf("audit file: %d priced, %d unpriced; want 1 and 1", priced, unpriced)
	}
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
