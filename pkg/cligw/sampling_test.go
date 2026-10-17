package cligw

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRequestSamplingCoversEveryRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{name: "none", body: `{"model":"L4","messages":[]}`},
		{name: "openai", body: `{"model":"L4","temperature":0.2,"seed":7,"messages":[]}`, want: "temperature,seed"},
		{name: "openai top_p", body: `{"model":"L4","top_p":0.9}`, want: "top_p"},
		{name: "anthropic", body: `{"model":"L4","max_tokens":8,"temperature":0,"top_k":40,"messages":[]}`, want: "temperature,top_k"},
		{name: "ollama options", body: `{"model":"tiny","options":{"seed":1,"top_k":5,"num_ctx":4096}}`, want: "top_k,seed"},
		{name: "ollama and top level", body: `{"model":"tiny","temperature":1,"options":{"seed":1}}`, want: "temperature,seed"},
		{name: "all four", body: `{"seed":1,"top_p":1,"top_k":1,"temperature":1}`, want: "temperature,top_k,top_p,seed"},
		// A parameter the caller explicitly sent as null asked for nothing,
		// so reporting it as dropped would be a false alarm.
		{name: "explicit null", body: `{"model":"L4","temperature":null,"seed":3}`, want: "seed"},
		{name: "not an object", body: `[1,2]`},
		{name: "not json", body: `broken`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IgnoredValue(RequestSampling([]byte(tc.body))); got != tc.want {
				t.Fatalf("RequestSampling(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// No CLI tool honors a sampling parameter, so the ignored list is whatever the
// request carried — stated as a test so a tool that grows a decoding flag has
// to come through HonoredSampling and not a silent exception somewhere else.
func TestIgnoredSamplingDropsEverythingACLISeatCannotHonor(t *testing.T) {
	body := []byte(`{"model":"L4","temperature":0.2,"seed":7}`)
	for _, tool := range []string{"", "claude", "codex", "muse", "agy"} {
		if got := IgnoredValue(IgnoredSampling(tool, body)); got != "temperature,seed" {
			t.Fatalf("IgnoredSampling(%q) = %q", tool, got)
		}
		if honored := HonoredSampling(tool); len(honored) != 0 {
			t.Fatalf("HonoredSampling(%q) = %v, want none", tool, honored)
		}
	}
}

// The door answers a CLI seat, so the sampling controls the caller sent were
// dropped: the response has to say which ones, on every surface the door
// accepts and before the first byte of a streamed answer.
func TestServerReportsIgnoredSamplingParams(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		want             string
		sse              bool
	}{
		{
			name: "openai temperature and seed",
			path: "/v1/chat/completions",
			body: `{"model":"L4","temperature":0.2,"seed":7,"messages":[{"role":"user","content":"say ok"}]}`,
			want: "temperature,seed",
		},
		{
			name: "openai streaming",
			path: "/v1/chat/completions",
			body: `{"model":"L4","stream":true,"top_p":0.5,"seed":7,"messages":[{"role":"user","content":"say ok"}]}`,
			want: "top_p,seed",
			sse:  true,
		},
		{
			name: "anthropic shape",
			path: "/v1/messages",
			body: `{"model":"L4","max_tokens":16,"temperature":0.3,"top_k":40,"messages":[{"role":"user","content":"say ok"}]}`,
			want: "temperature,top_k",
		},
		{
			name: "nothing to report",
			path: "/v1/chat/completions",
			body: `{"model":"L4","messages":[{"role":"user","content":"say ok"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})
			resp := ts.do(t, http.MethodPost, tc.path, ts.Token(), tc.body)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s = %s: %s", tc.path, resp.Status, body)
			}
			// On a streamed answer the header is only in the response at all
			// because it was set before the first chunk was written.
			if tc.sse && !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("Content-Type = %q, want an SSE stream", resp.Header.Get("Content-Type"))
			}
			if got := resp.Header.Get(IgnoredHeader); got != tc.want {
				t.Fatalf("%s = %q, want %q", IgnoredHeader, got, tc.want)
			}
		})
	}
}
