package openai

import (
	"encoding/json"
	"testing"
)

func TestEffectiveMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  ChatRequest
		want int
	}{
		{name: "max tokens", req: ChatRequest{MaxTokens: 10, MaxCompletionTokens: 20}, want: 10},
		{name: "max completion tokens", req: ChatRequest{MaxCompletionTokens: 20}, want: 20},
		{name: "default", req: ChatRequest{}, want: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.EffectiveMaxTokens(); got != tc.want {
				t.Fatalf("EffectiveMaxTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFlattenContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `"hello"`, want: "hello"},
		{name: "parts", raw: `[{"type":"text","text":"hello "},{"type":"image_url"},{"type":"text","text":"world"}]`, want: "hello world"},
		{name: "invalid", raw: `{`, want: ""},
		{name: "empty", raw: ``, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FlattenContent(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("FlattenContent(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
