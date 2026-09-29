package broker

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Sprint #324 S5: a slash= request runs a tool command; it is never served
// from a sticky binding, so benchmark identities stay pure.

func TestSlashRefusedOnStickyBinding(t *testing.T) {
	h := newHarness(t, nil)
	// An existing CLI binding.
	resp, out := h.do("POST", "/v1/chat/completions", chat("L5"), map[string]string{StickyHeader: "arm-a; uses=0"})
	if resp.StatusCode != 200 {
		t.Fatalf("create binding: %d %v", resp.StatusCode, out)
	}
	_, before, _ := h.cli.snap()

	for name, req := range map[string]struct {
		path string
		hdr  map[string]string
	}{
		"path key":       {"/sticky/arm-a/v1/chat/completions", map[string]string{"X-Bashy-Filter": "slash=plan"}},
		"header key":     {"/v1/chat/completions", map[string]string{"X-Bashy-Filter": "slash=plan", StickyHeader: "arm-a"}},
		"implicit spec":  {"/v1/chat/completions", map[string]string{"X-Bashy-Filter": "tool=claude,slash=plan", StickyHeader: "arm-new; uses=0"}},
		"anthropic path": {"/sticky/arm-a/v1/messages", map[string]string{"X-Bashy-Filter": "slash=plan"}},
	} {
		resp, out := h.do("POST", req.path, chat("L5"), req.hdr)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: slash on sticky = %d %v, want 400", name, resp.StatusCode, out)
		}
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "slash=plan") || !strings.Contains(msg, "sticky") {
			t.Fatalf("%s: message %q", name, msg)
		}
	}
	if _, after, _ := h.cli.snap(); after["model"] != before["model"] || len(after) != len(before) {
		t.Fatal("a refused slash request reached cligw")
	}
	// A binding cannot be created with a slash filter either.
	resp, out = h.do("POST", "/v1/sticky", StickySpec{Key: "arm-s", Model: "L5", Filter: "slash=plan"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with slash filter = %d %v, want 400", resp.StatusCode, out)
	}
	// Ordinary sticky use is unchanged.
	resp, _ = h.do("POST", "/sticky/arm-a/v1/chat/completions", chat("x"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("plain sticky use = %d", resp.StatusCode)
	}
}

func TestSlashUnboundGoesToCLIAndLocalModelIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do("POST", "/v1/chat/completions", chat("L5"), map[string]string{"X-Bashy-Filter": "slash=plan"})
	if resp.StatusCode != 200 || resp.Header.Get(BackendHeader) != BackendCLI {
		t.Fatalf("unbound slash = %d backend %q", resp.StatusCode, resp.Header.Get(BackendHeader))
	}
	resp, out := h.do("POST", "/v1/chat/completions", chat("llama3.2:3b"), map[string]string{"X-Bashy-Filter": "slash=plan"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("slash on a local model = %d %v, want 400", resp.StatusCode, out)
	}
}

// slashModelsCLI answers /v1/models like cligw would for a slash listing.
type slashModelsCLI struct {
	fakeCLI
	gotFilter, gotQuery string
}

func (c *slashModelsCLI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/models" {
		c.fakeCLI.ServeHTTP(w, r)
		return
	}
	c.gotFilter, c.gotQuery = r.Header.Get("X-Bashy-Filter"), r.URL.RawQuery
	if strings.Contains(r.URL.RawQuery, "slash=nope") {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"cligw: no tool declares command \"nope\""}`)
		return
	}
	io.WriteString(w, `{"object":"list","data":[{"id":"claude-opus5","object":"model"}]}`)
}

func TestSlashModelsForwardsFilterAndSkipsLocal(t *testing.T) {
	cli := &slashModelsCLI{}
	h := newHarness(t, func(o *Options) { o.CLI = cli })
	resp, out := h.do("GET", "/v1/models?slash=plan", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	data := out["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "claude-opus5" {
		t.Fatalf("slash listing must hold only capable agents, no local models: %v", data)
	}
	if cli.gotQuery != "slash=plan" {
		t.Fatalf("query not forwarded: %q", cli.gotQuery)
	}
	h.do("GET", "/v1/models", nil, map[string]string{"X-Bashy-Filter": "slash=plan"})
	if cli.gotFilter != "slash=plan" {
		t.Fatalf("filter header not forwarded: %q", cli.gotFilter)
	}
	resp, out = h.do("GET", "/v1/models?slash=nope", nil, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out["error"].(string), "nope") {
		t.Fatalf("cligw's 400 swallowed: %d %v", resp.StatusCode, out)
	}
}
