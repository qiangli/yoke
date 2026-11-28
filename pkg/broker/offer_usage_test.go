package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
)

func TestOfferMeteredCalls(t *testing.T) {
	dir := t.TempDir()
	peers := filepath.Join(t.TempDir(), "peers.json")
	b, err := New(t.Context(), Options{Token: testToken, OffersDir: dir, PeersFile: peers})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := MintPeerCredential(peers, "caller", ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = SetPeerCommands(peers, "caller", []string{"command"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	offerRunner.Lock()
	old := offerRunner.run
	offerRunner.run = func(_ context.Context, _ OfferRecord, _ map[string]any) (json.RawMessage, any, error) {
		return json.RawMessage(`{"text":"{\"seconds\":1.75}"}`), CallUsage{Input: 4, Output: 3}, nil
	}
	offerRunner.Unlock()
	t.Cleanup(func() { RegisterOfferRunner(old) })

	for _, tc := range []struct {
		name, class, unit, principal string
		units                        float64
		limit                        uint64
	}{
		{"command", "tool", "call", "peer:caller", 1, 1},
		{"command", "tool", "call", "owner", 1, 1},
		{"audio", "tool", "seconds", "member:member@example.test", 1.75, 0},
		{"model", "model", "token", "member:member@example.test", 7, 0},
		{"worker", "agent", "token", "owner", 7, 0},
	} {
		t.Run(tc.name+"/"+tc.principal, func(t *testing.T) {
			def, _ := json.Marshal(offerDefinition{Name: tc.name, Class: tc.class, Unit: tc.unit})
			o := OfferRecord{Name: tc.name, Digest: "definition-" + tc.name, Definition: def, SSOSecret: "secret", Limit: tc.limit}
			if err := SaveOffer(dir, o); err != nil {
				t.Fatal(err)
			}
			call := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/offers/"+tc.name, strings.NewReader(`{"args":{}}`))
				switch tc.principal {
				case "owner":
					r.Header.Set("Authorization", "Bearer "+testToken)
				case "peer:caller":
					r.Header.Set("Authorization", "Bearer "+cred)
				default:
					r = signedOfferRequest("POST", r.URL.Path, tc.name, o.SSOSecret)
					r.Body = io.NopCloser(strings.NewReader(`{"args":{}}`))
				}
				w := httptest.NewRecorder()
				b.ServeHTTP(w, r)
				return w
			}
			w := call()
			var result struct{ Usage OfferUsage }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			want := OfferUsage{Tool: tc.name, Class: tc.class, Unit: tc.unit, Units: tc.units, Digest: o.Digest}
			records := b.audit.snapshot()
			rec := records[len(records)-1]
			if result.Usage != want || (OfferUsage{rec.Tool, rec.OfferClass, rec.Unit, rec.Units, rec.Digest}) != want || rec.Principal != tc.principal {
				t.Fatalf("wire %+v, record %+v, want %+v", result.Usage, rec, want)
			}
			if tc.principal == "peer:caller" {
				if w := call(); w.Code != http.StatusTooManyRequests {
					t.Fatalf("second command %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestInferenceOfferUsageHeader(t *testing.T) {
	dir := t.TempDir()
	o := OfferRecord{Name: "sample", Digest: "sample-digest", Definition: json.RawMessage(`{"name":"sample","class":"model","unit":"token"}`), SSOSecret: "secret"}
	if err := SaveOffer(dir, o); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(opts *Options) { opts.OffersDir = dir })
	for _, stream := range []bool{false, true} {
		r := signedOfferRequest("POST", "/v1/chat/completions", "sample", o.SSOSecret)
		body, _ := json.Marshal(map[string]any{"model": "sample", "stream": stream})
		r.Body = io.NopCloser(bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.b.ServeHTTP(w, r)
		resp := w.Result()
		raw := resp.Header.Get(OfferUsageHeader)
		if stream {
			raw = resp.Trailer.Get(OfferUsageHeader)
		}
		var usage OfferUsage
		if w.Code != 200 || json.Unmarshal([]byte(raw), &usage) != nil || usage != (OfferUsage{"llm", "model", "token", 3, o.Digest}) {
			t.Fatalf("stream %v status %d usage %q body %s", stream, w.Code, raw, w.Body.String())
		}
		recs := h.b.audit.snapshot()
		rec := recs[len(recs)-1]
		if rec.Digest != usage.Digest || rec.Units != usage.Units || rec.Tool != "llm" {
			t.Fatalf("record %+v", rec)
		}
	}
}

func TestFileStreamUsageAndLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	share := t.TempDir()
	if err := os.WriteFile(filepath.Join(share, "sample"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	writeShareRecord(t, home, "docs", map[string]any{"name": "docs", "path": share})
	dir := t.TempDir()
	o := OfferRecord{Name: "files-docs", Definition: json.RawMessage(`{"name":"files-docs","class":"tool","unit":"bytes"}`), Digest: "docs", Limit: 7, SSOSecret: "secret"}
	if err := SaveOffer(dir, o); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(opts *Options) { opts.OffersDir = dir })
	// Inline reads and subsequent byte streams share one counter.
	req := signedOfferRequest("POST", "/offers/files-docs", "files-docs", o.SSOSecret)
	req.Body = io.NopCloser(strings.NewReader(`{"args":{"op":"read","path":"sample","length":2}}`))
	w := httptest.NewRecorder()
	h.b.ServeHTTP(w, req)
	var result struct{ Usage OfferUsage }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Usage != (OfferUsage{"files-docs", "tool", "bytes", 2, "docs"}) {
		t.Fatalf("inline response %d %s", w.Code, w.Body.String())
	}
	// A range request's usage is available only after the bytes are written.
	for i, want := range []int{206, 429} {
		r := signedOfferRequest("GET", "/files/docs/sample", "files-docs", o.SSOSecret)
		r.URL, _ = url.Parse(h.server.URL + r.URL.Path)
		r.Header.Set("Range", "bytes=2-6")
		r.RequestURI = ""
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatalf("request %d: status %d, %s, %v", i, resp.StatusCode, data, err)
		}
		if i == 0 {
			var u OfferUsage
			if json.Unmarshal([]byte(resp.Trailer.Get(OfferUsageHeader)), &u) != nil || u != (OfferUsage{"files-docs", "tool", "bytes", 5, "docs"}) || string(data) != "23456" {
				t.Fatalf("body %q, usage %+v, trailer %v", data, u, resp.Trailer)
			}
			rec := h.lastRecord(2)
			if rec.Units != u.Units || rec.Digest != u.Digest || rec.Bytes != 5 {
				t.Fatalf("record %+v", rec)
			}
		}
	}
}

type usageAPI string

func (u usageAPI) ResolveAPI(context.Context, string) (APIUpstream, bool, error) {
	return APIUpstream{BaseURL: string(u)}, true, nil
}

func TestOfferAPIUsageOnOwnerAndPeerInference(t *testing.T) {
	dir := t.TempDir()
	def := json.RawMessage(`{"name":"sample","class":"model","unit":"token"}`)
	if err := SaveOffer(dir, OfferRecord{Name: "sample", Definition: def, Digest: "api", SSOSecret: "secret", Limit: 3}); err != nil {
		t.Fatal(err)
	}
	peers := filepath.Join(t.TempDir(), "peers.json")
	cred, err := MintPeerCredential(peers, "caller", ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPeerCommands(peers, "caller", []string{"sample"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	h := newHarness(t, func(o *Options) { o.OffersDir = dir; o.PeersFile = peers; o.API = usageAPI(upstream.URL) })
	for i, key := range []string{testToken, cred, cred} {
		headers := map[string]string{"Authorization": "Bearer " + key}
		if key == testToken {
			headers[coopauth.HdrForwardedPrefix] = "/app/offer-sample"
		}
		resp, _ := h.do("POST", "/v1/chat/completions", chat("sample"), headers)
		if i == 2 {
			if resp.StatusCode != 429 {
				t.Fatalf("exhausted peer quota: %d", resp.StatusCode)
			}
			continue
		}
		var u OfferUsage
		if resp.StatusCode != 200 || json.Unmarshal([]byte(resp.Header.Get(OfferUsageHeader)), &u) != nil || u != (OfferUsage{"llm", "model", "token", 3, "api"}) {
			t.Fatalf("call %d: status %d header %v", i, resp.StatusCode, resp.Header)
		}
	}
}
