package broker

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/cligw"
)

// A CLI seat has no decoding knobs, so the door reports what it dropped —
// on the response and on the run record, which is what makes an audited
// benchmark row honest about how it was sampled.
func TestIgnoredSamplingIsReportedForCLISeats(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		body       map[string]any
		want       string
	}{
		{
			name: "openai temperature and seed",
			path: "/v1/chat/completions",
			body: withParams(chat("L5"), map[string]any{"temperature": 0.2, "seed": 7}),
			want: "temperature,seed",
		},
		{
			name: "openai streaming",
			path: "/v1/chat/completions",
			body: withParams(chat("L5"), map[string]any{"stream": true, "top_k": 40, "top_p": 0.5}),
			want: "top_k,top_p",
		},
		{
			name: "anthropic shape",
			path: "/v1/messages",
			body: withParams(chat("L5"), map[string]any{"max_tokens": 16, "temperature": 0.3, "top_k": 40}),
			want: "temperature,top_k",
		},
		{
			name: "nothing to report",
			path: "/v1/chat/completions",
			body: chat("L5"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			resp, _ := h.do("POST", tc.path, tc.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%s = %d", tc.path, resp.StatusCode)
			}
			if resp.Header.Get(BackendHeader) != BackendCLI {
				t.Fatalf("%s = %q, want a CLI seat", BackendHeader, resp.Header.Get(BackendHeader))
			}
			if got := resp.Header.Get(cligw.IgnoredHeader); got != tc.want {
				t.Fatalf("%s = %q, want %q", cligw.IgnoredHeader, got, tc.want)
			}
			if rec := h.lastRecord(1); rec.Ignored != tc.want {
				t.Fatalf("record ignored = %q, want %q", rec.Ignored, tc.want)
			}
		})
	}
}

// A local model honors all four, so nothing was dropped and the header must
// be absent — on the OpenAI surface and on Ollama's own.
func TestIgnoredSamplingIsAbsentForLocalModels(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{
			name: "openai surface",
			path: "/v1/chat/completions",
			body: withParams(chat("llama3.2:3b"), map[string]any{"temperature": 0.2, "seed": 7}),
		},
		{
			name: "ollama native options",
			path: "/api/chat",
			body: withParams(chat("llama3.2:3b"), map[string]any{"options": map[string]any{"temperature": 0.2, "seed": 7, "top_k": 40}}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			resp, _ := h.do("POST", tc.path, tc.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%s = %d", tc.path, resp.StatusCode)
			}
			if resp.Header.Get(BackendHeader) != BackendLocal {
				t.Fatalf("%s = %q, want the local engine", BackendHeader, resp.Header.Get(BackendHeader))
			}
			if got := resp.Header.Get(cligw.IgnoredHeader); got != "" {
				t.Fatalf("%s = %q on a model that honors them", cligw.IgnoredHeader, got)
			}
			if rec := h.lastRecord(1); rec.Ignored != "" {
				t.Fatalf("record ignored = %q, want empty", rec.Ignored)
			}
		})
	}
}

// The held-worker path (bind=worker/reset=none) serves a CLI turn without
// going through cligw's HTTP handler, so it needs the door's own header.
func TestIgnoredSamplingIsReportedOnStickyWorkerTurns(t *testing.T) {
	h := newHarness(t, nil)
	h.cli.enableSticky("reply-a")
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	body := withParams(chat("L5"), map[string]any{"temperature": 0.2, "seed": 7})
	resp, out := h.do("POST", "/sticky/conv/v1/chat/completions", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("sticky turn = %d: %v", resp.StatusCode, out)
	}
	if got := resp.Header.Get(cligw.IgnoredHeader); got != "temperature,seed" {
		t.Fatalf("%s = %q", cligw.IgnoredHeader, got)
	}
	if rec := h.lastRecord(1); rec.Ignored != "temperature,seed" || !strings.Contains(rec.Sticky, "conv") {
		t.Fatalf("record = %+v", rec)
	}
}

// withParams returns body with extra fields set, so a case can state only the
// sampling controls it is about.
func withParams(body, extra map[string]any) map[string]any {
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// The capture record is what a replayed or scored turn is read back from, so
// it carries the same list: a captured answer is evidence of a run whose
// sampling parameters went nowhere.
func TestIgnoredSamplingIsKeptOnTheCaptureRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	session := "s-abcdef123456"
	h := newHarness(t, func(o *Options) { o.Capture = true })
	body := withParams(chat("L5"), map[string]any{"temperature": 0.2, "seed": 7})
	if resp, _ := h.do("POST", "/s/"+session+"/v1/chat/completions", body, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	turns, err := readCaptureTurns(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Ignored != "temperature,seed" {
		t.Fatalf("capture turns = %+v", turns)
	}
}
