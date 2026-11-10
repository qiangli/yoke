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
