package cligw

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// Sampling controls a CLI seat cannot honor.
//
// A vendor coding CLI is a one-shot exec that takes a prompt, a model id and
// (sometimes) a reasoning effort. None of them exposes a decoding knob, so
// temperature/top_k/top_p/seed are dropped where the request becomes a
// prompt (RenderCompletionPrompt) — the caller's values never reach a token
// sampler. A local Ollama model honors all four, so the same request answered
// by the two backends is sampled two different ways.
//
// Silently is the part that costs: a caller pinning seed for a reproducible
// run, or temperature=0 for a deterministic one, gets neither and has no way
// to know. So the door REPORTS the drop on the response instead — it does not
// refuse the request and it does not change the route (capability filtering is
// post-1.0).

const (
	// IgnoredHeader lists the request parameters the serving backend could
	// not honor, in SamplingParams order, comma separated.
	IgnoredHeader = "X-Bashy-Ignored"
)

// SamplingParams are the sampling controls the door accepts, in the canonical
// order IgnoredHeader reports them. The order is fixed so the header value is
// deterministic for a given request, whatever order the caller's JSON used.
var SamplingParams = []string{"temperature", "top_k", "top_p", "seed"}

// RequestSampling returns the sampling parameters present in a request body,
// in SamplingParams order. It covers the three request shapes the door
// accepts: OpenAI chat and Anthropic messages carry them at the top level,
// Ollama's native API nests them under "options". A parameter explicitly sent
// as null is not carried.
func RequestSampling(body []byte) []string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	var options map[string]json.RawMessage
	if raw, ok := payload["options"]; ok {
		_ = json.Unmarshal(raw, &options)
	}
	var out []string
	for _, name := range SamplingParams {
		if jsonPresent(payload[name]) || jsonPresent(options[name]) {
			out = append(out, name)
		}
	}
	return out
}

// HonoredSampling returns the sampling parameters a seat running tool can
// honor. It is empty for every tool: see the package comment above. This is a
// function rather than a constant so a vendor CLI that grows a decoding flag
// is one row of truth away from being reported correctly, and so callers read
// the capability instead of assuming it.
func HonoredSampling(tool string) []string { return nil }

// IgnoredSampling returns the sampling parameters in body that a CLI seat
// running tool cannot honor, in SamplingParams order. tool may be "" when the
// caller knows only that the request goes to some CLI seat; no tool honors
// any of them, so the answer is the same.
func IgnoredSampling(tool string, body []byte) []string {
	honored := HonoredSampling(tool)
	if len(honored) == 0 {
		return RequestSampling(body)
	}
	var out []string
	for _, name := range RequestSampling(body) {
		if !slices.Contains(honored, name) {
			out = append(out, name)
		}
	}
	return out
}

// IgnoredValue renders an ignored-parameter list as an IgnoredHeader value;
// "" when nothing was dropped, so the header is simply absent.
func IgnoredValue(ignored []string) string { return strings.Join(ignored, ",") }

func jsonPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}
