package saasclients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/external/gws"
)

const canary = "xoxb-CANARY-SECRET-0123456789"

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func credFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdmissionRecords(t *testing.T) {
	want := map[string]string{"sf": "Apache-2.0", "stripe": "Apache-2.0", "twilio": "MIT", "slack": "Apache-2.0"}
	for n, lic := range want {
		c, ok := Lookup(n)
		if !ok || c.License != lic || len(c.Commit) != 40 || c.Version == "" || c.Repo == "" || len(c.Digests) == 0 {
			t.Errorf("%s: bad admission record %+v", n, c)
		}
		for a, d := range c.Digests {
			if len(d) != 64 {
				t.Errorf("%s: asset %s digest length %d", n, a, len(d))
			}
		}
	}
	if len(Names()) != 4 {
		t.Errorf("names = %v", Names())
	}
}

func TestAssetMatchUnambiguous(t *testing.T) {
	for _, name := range []string{"stripe", "slack"} {
		c := clients[name]
		for _, p := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}, {"windows", "amd64"}} {
			n := 0
			for a := range c.Digests {
				if AssetMatch(name)(a, p[0], p[1]) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s %v matched %d assets, want exactly 1", name, p, n)
			}
		}
	}
	if AssetMatch("twilio")("twilio-7.2.0.exe", "windows", "amd64") || AssetMatch("sf")("x", "linux", "amd64") {
		t.Error("host-only clients must not match any asset")
	}
	if _, err := Spec("twilio", ""); err == nil {
		t.Error("twilio must be host-only")
	}
	if s, err := Spec("stripe", ""); err != nil || s.Version != "v1.54.0" {
		t.Errorf("stripe spec = %+v, %v", s, err)
	}
}

func TestCapabilityScopeNotAssumed(t *testing.T) {
	// Slack CLI is app tooling: messaging must be the API adapter, not the CLI.
	for _, n := range []string{"message-post", "history", "channel-list", "message-delete"} {
		if o, ok := LookupOp("slack", n); !ok || o.Via != ViaAPI {
			t.Errorf("slack %s must be via api, got %+v", n, o)
		}
	}
	if o, _ := LookupOp("sf", "record-upsert"); o.Via != ViaAPI {
		t.Error("sf single-record upsert is not in the CLI; must be REST")
	}
	if o, _ := LookupOp("stripe", "create"); o.Idem != IdemProvider {
		t.Error("stripe create must use provider idempotency")
	}
	for _, o := range Ops {
		if o.Effect != EffectRead && o.Idem == IdemNone {
			t.Errorf("mutation %s:%s has no idempotency strategy", o.Client, o.Name)
		}
	}
}

func TestResolveScopeExplicit(t *testing.T) {
	f := credFile(t, "whatever")
	_, e := ResolveScope("slack", envOf(nil))
	if e == nil || e.Code != gws.CodeMissingCredentials {
		t.Fatalf("no target: %v", e)
	}
	_, e = ResolveScope("slack", envOf(map[string]string{"SLACK_WORKSPACE": "T1"}))
	if e == nil || e.Code != gws.CodeMissingCredentials {
		t.Fatalf("no cred: %v", e)
	}
	sc, e := ResolveScope("slack", envOf(map[string]string{"SLACK_WORKSPACE": "T1", "SLACK_TOKEN_FILE": f, "SLACK_SANDBOX": "1"}))
	if e != nil || !sc.Sandbox || sc.Target != "T1" {
		t.Fatalf("scope: %+v %v", sc, e)
	}
	if rs := sc.RedactedScope(); strings.Contains(rs, filepath.Dir(f)) || !strings.Contains(rs, "file:cred") {
		t.Errorf("redacted scope leaks path: %s", rs)
	}
	if _, e = ResolveScope("slack", envOf(map[string]string{"SLACK_WORKSPACE": "T1", "SLACK_TOKEN_FILE": t.TempDir()})); e == nil || e.Code != gws.CodeInvalidInput {
		t.Errorf("directory cred: %v", e)
	}
	if _, e = ResolveScope("nope", envOf(nil)); e == nil {
		t.Error("unknown client accepted")
	}
}

func TestStripeKeyModes(t *testing.T) {
	env := func(k string) map[string]string {
		return map[string]string{"STRIPE_ACCOUNT": "acct_1", "STRIPE_API_KEY_FILE": credFile(t, k)}
	}
	if _, e := ResolveScope("stripe", envOf(env("sk_live_ABC"))); e == nil || e.Code != gws.CodePermissionDenied {
		t.Errorf("live key: %v", e)
	}
	if _, e := ResolveScope("stripe", envOf(env("garbage"))); e == nil || e.Code != gws.CodeInvalidInput {
		t.Errorf("garbage key: %v", e)
	}
	sc, e := ResolveScope("stripe", envOf(env("sk_test_ABC\n")))
	if e != nil || !sc.Sandbox {
		t.Errorf("test key: %+v %v", sc, e)
	}
}

func TestModeAdmission(t *testing.T) {
	cases := []struct {
		client string
		argv   []string
		want   Mode
	}{
		{"stripe", []string{"customers", "list", "--limit", "3"}, ModeShortLived},
		{"stripe", []string{"listen", "--forward-to", "localhost:4242/webhook"}, ModeWebhookForward},
		{"stripe", []string{"login"}, ModeInteractiveAuth},
		{"twilio", []string{"serverless:start"}, ModeResident},
		{"twilio", []string{"phone-numbers:update", "PN1", "--sms-url=https://x"}, ModeWebhookServe},
		{"twilio", []string{"api:core:messages:list"}, ModeShortLived},
		{"slack", []string{"run"}, ModeResident},
		{"slack", []string{"manifest", "info"}, ModeShortLived},
		{"sf", []string{"apex", "tail", "log"}, ModeResident},
		{"sf", []string{"org", "login", "web"}, ModeInteractiveAuth},
		{"sf", []string{"data", "query", "--query", "SELECT Id FROM Account"}, ModeShortLived},
	}
	for _, c := range cases {
		m, e := AdmitArgs(c.client, c.argv)
		if m != c.want {
			t.Errorf("%s %v: mode %s, want %s", c.client, c.argv, m, c.want)
		}
		if (m == ModeShortLived) != (e == nil) {
			t.Errorf("%s %v: admit err = %v", c.client, c.argv, e)
		}
		if e != nil && e.Exit() != 2 {
			t.Errorf("refusal must exit 2, got %d", e.Exit())
		}
	}
}

func TestMutationValidation(t *testing.T) {
	sb := Scope{Client: "slack", Target: "T1", Sandbox: true}
	prod := Scope{Client: "slack", Target: "T1"}
	if _, e := (Mutation{Scope: prod, Op: "message-post"}).Validate(); e == nil || e.Code != gws.CodePermissionDenied {
		t.Errorf("non-sandbox message: %v", e)
	}
	if _, e := (Mutation{Scope: sb, Op: "history"}).Validate(); e == nil || e.Code != gws.CodeInvalidInput {
		t.Errorf("read as mutation: %v", e)
	}
	if _, e := (Mutation{Scope: sb, Op: "nope"}).Validate(); e == nil || e.Code != gws.CodeUnsupported {
		t.Errorf("unknown op: %v", e)
	}
	if _, e := (Mutation{Scope: sb, Op: "message-post", Payload: make([]byte, gws.MaxPayloadBytes+1)}).Validate(); e == nil || e.Code != gws.CodeOutputLimit {
		t.Errorf("oversize: %v", e)
	}
	p1, _ := Mutation{Scope: sb, Op: "message-post", Payload: []byte("a")}.Validate()
	p2, _ := Mutation{Scope: sb, Op: "message-post", Payload: []byte("a")}.Validate()
	p3, _ := Mutation{Scope: sb, Op: "message-post", Payload: []byte("b")}.Validate()
	if p1.Key == "" || p1.Key != p2.Key || p1.Key == p3.Key {
		t.Errorf("derived keys not deterministic/distinct: %s %s %s", p1.Key, p2.Key, p3.Key)
	}
	if p, _ := (Mutation{Scope: sb, Op: "message-post", Key: "k"}).Validate(); p.Key != "k" {
		t.Error("explicit key overridden")
	}
	st := Scope{Client: "stripe", Target: "acct_1"}
	if _, e := (Mutation{Scope: st, Op: "payment-confirm"}).Validate(); e == nil {
		t.Error("stripe payment on undeclared sandbox accepted")
	}
}

// fakeSlack is a loopback Slack Web API with post/delete/history and cleanup.
type fakeSlack struct {
	mu    sync.Mutex
	msgs  map[string]string
	posts int
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+canary {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "chat.postMessage":
		f.posts++
		ts := "1700000000.00000" + string(rune('0'+f.posts))
		f.msgs[ts] = in["text"]
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
	case "chat.delete":
		if _, ok := f.msgs[in["ts"]]; !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
			return
		}
		delete(f.msgs, in["ts"])
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case "limited":
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
	case "huge":
		_, _ = w.Write([]byte(`{"ok":true,"pad":"` + strings.Repeat("x", 2048) + `"}`))
	case "scope":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope", "detail": canary})
	}
}

func TestSlackSandboxPostCleanup(t *testing.T) {
	f := &fakeSlack{msgs: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := API{Client: "slack", BaseURL: srv.URL, Token: canary}
	sb := Mutation{Scope: Scope{Client: "slack", Target: "T1", Sandbox: true}, Key: "k1"}
	var l Ledger

	// dry run validates and sends nothing
	dry := sb
	dry.DryRun = true
	if _, e := SlackPost(context.Background(), a, &l, dry, "C1", "hi"); e != nil || f.posts != 0 {
		t.Fatalf("dry run: %v posts=%d", e, f.posts)
	}
	ts, e := SlackPost(context.Background(), a, &l, sb, "C1", "hi")
	if e != nil || ts == "" || len(f.msgs) != 1 {
		t.Fatalf("post: ts=%q err=%v", ts, e)
	}
	// replayed key must not post again
	if _, e := SlackPost(context.Background(), a, &l, sb, "C1", "hi"); e == nil || e.Code != gws.CodePartialMutation || f.posts != 1 {
		t.Fatalf("replay: %v posts=%d", e, f.posts)
	}
	// cleanup
	body, _ := json.Marshal(map[string]string{"channel": "C1", "ts": ts})
	if _, e := a.Do(context.Background(), "POST", "chat.delete", nil, body, nil); e != nil || len(f.msgs) != 0 {
		t.Fatalf("cleanup: %v left=%d", e, len(f.msgs))
	}
	if _, e := a.Do(context.Background(), "POST", "chat.delete", nil, body, nil); e == nil || e.Code != gws.CodeNotFound {
		t.Fatalf("second delete: %v", e)
	}
	// non-sandbox never reaches the network
	before := f.posts
	if _, e := SlackPost(context.Background(), a, &l, Mutation{Scope: Scope{Client: "slack", Target: "T1"}, Key: "k2"}, "C1", "x"); e == nil || f.posts != before {
		t.Fatalf("non-sandbox post reached provider: %v", e)
	}
}

func TestAdapterTypedErrorsBoundsAndRedaction(t *testing.T) {
	f := &fakeSlack{msgs: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	a := API{Client: "slack", BaseURL: srv.URL, Token: canary}

	if _, e := (API{Client: "slack", BaseURL: srv.URL, Token: "wrong"}).Do(ctx, "POST", "chat.postMessage", nil, nil, nil); e == nil || e.Code != gws.CodeExpiredCredentials {
		t.Errorf("invalid_auth: %v", e)
	}
	_, e := a.Do(ctx, "POST", "limited", nil, nil, nil)
	if e == nil || e.Code != gws.CodeRateLimit || e.RetryAfter != "7" || !e.Retryable {
		t.Errorf("rate limit: %+v", e)
	}
	small := a
	small.MaxBytes = 1024
	if _, e := small.Do(ctx, "POST", "huge", nil, nil, nil); e == nil || e.Code != gws.CodeOutputLimit {
		t.Errorf("bounded output: %v", e)
	}
	if _, e := a.Do(ctx, "POST", "scope", nil, nil, nil); e == nil || e.Code != gws.CodePermissionDenied {
		t.Errorf("missing_scope: %v", e)
	}
	if _, e := (API{Client: "slack"}).Do(ctx, "POST", "x", nil, nil, nil); e == nil || e.Code != gws.CodeMissingCredentials {
		t.Errorf("no endpoint/token: %v", e)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := a.Do(cctx, "POST", "chat.postMessage", nil, nil, nil); e == nil || e.Code != gws.CodeCancelled {
		t.Errorf("cancel: %v", e)
	}

	// canary: token never appears in any error surface, even when the provider echoes it
	for _, p := range []string{"scope", "limited", "chat.postMessage"} {
		_, e := (API{Client: "slack", BaseURL: srv.URL, Token: canary}).Do(ctx, "POST", p, nil, nil, map[string]string{"X-Echo": canary})
		if e != nil {
			var sb strings.Builder
			e.EmitError(&sb, "slack")
			if all := e.Message + sb.String() + e.ProviderCode; strings.Contains(all, canary) {
				t.Errorf("canary leaked via %s: %s", p, all)
			}
		}
	}
	args, n := gws.RedactArgs([]string{"slack", "--token", canary, "x"}, canary)
	if n == 0 || strings.Contains(strings.Join(args, " "), canary) {
		t.Errorf("argv canary: %v", args)
	}
}

// fakeSF is a loopback Salesforce REST sandbox keyed by external id.
func TestSalesforceUpsertIdempotentWithCleanup(t *testing.T) {
	var mu sync.Mutex
	rows := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // services data vXX sobjects Obj ...
		switch r.Method {
		case "PATCH":
			key := parts[5] + "/" + parts[6]
			_, existed := rows[key]
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			rows[key] = in
			if existed {
				w.WriteHeader(204)
			} else {
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"001X","success":true}`))
			}
		case "DELETE":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`[{"errorCode":"NOT_FOUND","message":"gone ` + canary + `"}]`))
		}
	}))
	defer srv.Close()
	a := API{Client: "sf", BaseURL: srv.URL, Token: canary}
	m := Mutation{Scope: Scope{Client: "sf", Target: "sandbox-org", Sandbox: true}}
	for i := 0; i < 2; i++ {
		if e := SalesforceUpsert(context.Background(), a, "v62.0", m, "Account", "Ext__c", "fixture-1", map[string]any{"Name": "Fixture"}); e != nil {
			t.Fatalf("upsert %d: %v", i, e)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("upsert not idempotent: %d rows", len(rows))
	}
	e := SalesforceDelete(context.Background(), a, "v62.0", m, "Account", "001X")
	if e == nil || e.Code != gws.CodeNotFound || e.ProviderCode != "NOT_FOUND" || strings.Contains(e.Message, canary) {
		t.Fatalf("delete error: %+v", e)
	}
}

func TestChildEnvTelemetryOptOut(t *testing.T) {
	if env := strings.Join(ChildEnv("sf", nil), " "); !strings.Contains(env, "SF_DISABLE_TELEMETRY=true") {
		t.Error(env)
	}
	if env := strings.Join(ChildEnv("stripe", []string{"A=1"}), " "); !strings.Contains(env, "STRIPE_CLI_TELEMETRY_OPTOUT=1") || !strings.Contains(env, "A=1") {
		t.Error(env)
	}
}
