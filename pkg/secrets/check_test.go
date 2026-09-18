package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syntheticKey stands in for a real credential everywhere in this file. No
// test may source a live key, and no assertion may print one.
const syntheticKey = "sk-proj-synthetic-check-fixture"

// probeRecorder is a fake provider: it answers each path with a canned status
// and records what was asked of it.
type probeRecorder struct {
	mu       sync.Mutex
	statuses map[string]int
	paths    []string
	bodies   []string
	headers  []http.Header
}

func newProbeRecorder(statuses map[string]int) *probeRecorder {
	return &probeRecorder{statuses: statuses}
}

func (p *probeRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.bodies = append(p.bodies, string(body))
		p.headers = append(p.headers, r.Header.Clone())
		code, ok := p.statuses[r.URL.Path]
		p.mu.Unlock()
		if !ok {
			code = http.StatusNotImplemented
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(server.Close)
	return server
}

func (p *probeRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// runProbe wires probeProvider against the fake provider, mirroring the real
// openai probe shape (cheap listing path, inference capability path).
func runProbe(t *testing.T, rec *probeRecorder, auth probeAuth, capability string) checkResult {
	t.Helper()
	server := rec.server(t)
	probe := providerProbe{
		baseURL:        server.URL,
		path:           "/models",
		capability:     capability,
		capabilityBody: openAICapabilityBody,
		auth:           auth,
	}
	if auth == anthropicAuth {
		probe.capabilityBody = anthropicCapabilityBody
	}
	return probeProvider(context.Background(), server.Client(), "openai", "openai", syntheticKey, probe)
}

func TestResolveProvider(t *testing.T) {
	tests := map[string]string{
		"dragon-moonshot": "moonshot",
		"dragon-deepseek": "deepseek",
		"dragon-openai":   "openai",
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := resolveProvider(name)
			if !ok || got != want {
				t.Fatalf("resolveProvider(%q) = %q, %v; want %q, true", name, got, ok, want)
			}
		})
	}
}

func TestLookupCheckKeyPrefersReachableVault(t *testing.T) {
	fv := newFakeVault(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-stale")
	fv.data["openai"] = "synthetic-current"

	got, err := lookupCheckKey(fv.cfg(), "openai")
	if err != nil {
		t.Fatalf("lookupCheckKey: %v", err)
	}
	if digest(got) != digest("synthetic-current") {
		t.Fatal("check key digest did not match the reachable vault")
	}
}

func TestLookupCheckKeyFallsBackToEnvironment(t *testing.T) {
	fv := newFakeVault(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-fallback")
	fv.server.Close()

	got, err := lookupCheckKey(fv.cfg(), "openai")
	if err != nil {
		t.Fatalf("lookupCheckKey fallback: %v", err)
	}
	if digest(got) != digest("synthetic-fallback") {
		t.Fatal("check key digest did not match the environment fallback")
	}
}

func TestLookupCheckKeyDoesNotMaskReachableVaultError(t *testing.T) {
	fv := newFakeVault(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-stale")

	got, err := lookupCheckKey(fv.cfg(), "openai")
	if err == nil {
		t.Fatal("lookupCheckKey unexpectedly masked a reachable vault error")
	}
	if got != "" {
		t.Fatal("lookupCheckKey returned an environment value after a reachable vault error")
	}
}

// TestProbeProviderAcceptedKeyIsNotInvalid is the regression this file exists
// for: a key the provider ACCEPTS must never come back INVALID. The
// scope-restricted case is the one that broke — OpenAI answers 403 on
// /v1/models for a project key restricted to inference, while the same key
// authenticates fine on /v1/chat/completions.
func TestProbeProviderAcceptedKeyIsNotInvalid(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses map[string]int
		wantCode int
	}{
		{
			name:     "fully scoped key lists models",
			statuses: map[string]int{"/models": http.StatusOK},
			wantCode: http.StatusOK,
		},
		{
			name:     "inference-scoped project key is refused on the listing path",
			statuses: map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusNotFound},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "provider rejects only the sentinel model",
			statuses: map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusBadRequest},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "provider accepts the capability request outright",
			statuses: map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusOK},
			wantCode: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runProbe(t, newProbeRecorder(tc.statuses), bearerAuth, "/chat/completions")
			if got.Status != statusValid {
				t.Fatalf("probeProvider() = %q (%s); want VALID for a key the provider accepts", got.Status, got.Reason)
			}
			if got.HTTPCode != tc.wantCode {
				t.Fatalf("probeProvider() http_code = %d; want %d", got.HTTPCode, tc.wantCode)
			}
		})
	}
}

// TestProbeProviderInvalidOnlyOnProviderRejection pins the other direction: a
// verifier that never says INVALID is as useless as one that always does.
func TestProbeProviderInvalidOnlyOnProviderRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses map[string]int
	}{
		{name: "listing path rejects the credential", statuses: map[string]int{"/models": http.StatusUnauthorized}},
		{name: "capability path rejects the credential", statuses: map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusUnauthorized}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runProbe(t, newProbeRecorder(tc.statuses), bearerAuth, "/chat/completions")
			if got.Status != statusInvalid {
				t.Fatalf("probeProvider() = %q (%s); want INVALID after an explicit 401", got.Status, got.Reason)
			}
		})
	}
}

// TestProbeProviderInconclusiveIsUnknownWithReason covers the claim the old
// code could not make: "I could not determine" is distinct from "the provider
// says this key is bad", and must carry why.
func TestProbeProviderInconclusiveIsUnknownWithReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statuses   map[string]int
		capability string
		wantSubstr string
	}{
		{
			name:       "rate limited",
			statuses:   map[string]int{"/models": http.StatusTooManyRequests},
			capability: "/chat/completions",
			wantSubstr: "rate limited",
		},
		{
			name:       "provider outage",
			statuses:   map[string]int{"/models": http.StatusInternalServerError},
			capability: "/chat/completions",
			wantSubstr: "provider error (HTTP 500)",
		},
		{
			name:       "unexpected status",
			statuses:   map[string]int{"/models": http.StatusTeapot},
			capability: "/chat/completions",
			wantSubstr: "unexpected status HTTP 418",
		},
		{
			name:       "forbidden on both paths",
			statuses:   map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusForbidden},
			capability: "/chat/completions",
			wantSubstr: "scope, not a bad key",
		},
		{
			name:       "forbidden with no capability probe configured",
			statuses:   map[string]int{"/models": http.StatusForbidden},
			capability: "",
			wantSubstr: "no capability probe is configured",
		},
		{
			name:       "rate limited on the capability path",
			statuses:   map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusTooManyRequests},
			capability: "/chat/completions",
			wantSubstr: "rate limited",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runProbe(t, newProbeRecorder(tc.statuses), bearerAuth, tc.capability)
			if got.Status != statusUnknown {
				t.Fatalf("probeProvider() = %q; want UNKNOWN for an inconclusive probe", got.Status)
			}
			if !strings.Contains(got.Reason, tc.wantSubstr) {
				t.Fatalf("probeProvider() reason = %q; want it to contain %q", got.Reason, tc.wantSubstr)
			}
		})
	}
}

// TestProbeProviderUnreachableIsUnknown: a network failure says nothing about
// the credential, so it must not be reported as a rejection.
func TestProbeProviderUnreachableIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	got := probeProvider(context.Background(), &http.Client{Timeout: time.Second}, "openai", "openai", syntheticKey, providerProbe{
		baseURL:        url,
		path:           "/models",
		capability:     "/chat/completions",
		capabilityBody: openAICapabilityBody,
		auth:           bearerAuth,
	})
	if got.Status != statusUnknown || got.HTTPCode != 0 {
		t.Fatalf("probeProvider() = status %q, code %d; want UNKNOWN, 0", got.Status, got.HTTPCode)
	}
	if !strings.Contains(got.Reason, "provider unreachable") {
		t.Fatalf("probeProvider() reason = %q; want it to name the unreachable provider", got.Reason)
	}
}

func TestProbeProviderTimeoutIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	got := probeProvider(context.Background(), client, "openai", "openai", syntheticKey, providerProbe{
		baseURL: server.URL,
		path:    "/models",
		auth:    bearerAuth,
	})
	if got.Status != statusUnknown || got.HTTPCode != 0 {
		t.Fatalf("probeProvider() = status %q, code %d; want UNKNOWN, 0", got.Status, got.HTTPCode)
	}
	if got.Reason == "" {
		t.Fatal("probeProvider() returned UNKNOWN with no reason")
	}
}

// TestProbeProviderSkipsCapabilityWhenListingAnswers keeps the escalation
// cheap: the inference path is only touched when the listing path was
// inconclusive about scope.
func TestProbeProviderSkipsCapabilityWhenListingAnswers(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTooManyRequests} {
		rec := newProbeRecorder(map[string]int{"/models": code, "/chat/completions": http.StatusOK})
		runProbe(t, rec, bearerAuth, "/chat/completions")
		if seen := rec.seen(); len(seen) != 1 || seen[0] != "/models" {
			t.Fatalf("listing status %d probed %v; want only /models", code, seen)
		}
	}
}

// TestProbeProviderCapabilityCarriesAuth: the escalated probe must present the
// same credential in the provider's own convention, or its 401 would be an
// artifact of the probe rather than a verdict about the key.
func TestProbeProviderCapabilityCarriesAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		auth   probeAuth
		header string
		want   string
	}{
		{name: "bearer", auth: bearerAuth, header: "Authorization", want: "Bearer " + syntheticKey},
		{name: "anthropic", auth: anthropicAuth, header: "X-Api-Key", want: syntheticKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newProbeRecorder(map[string]int{"/models": http.StatusForbidden, "/chat/completions": http.StatusNotFound})
			runProbe(t, rec, tc.auth, "/chat/completions")

			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.headers) != 2 {
				t.Fatalf("got %d requests; want 2 (listing then capability)", len(rec.headers))
			}
			if got := rec.headers[1].Get(tc.header); got != tc.want {
				t.Fatalf("capability probe %s header mismatch", tc.header)
			}
			if body := rec.bodies[1]; !strings.Contains(body, probeModel) {
				t.Fatalf("capability probe body = %q; want the sentinel model %q", body, probeModel)
			}
			if strings.Contains(rec.bodies[1], syntheticKey) {
				t.Fatal("capability probe body carried the credential")
			}
		})
	}
}

// TestCheckResultNeverCarriesTheKey: the verdict is operator-facing output
// that gets pasted into issues and logs. No path through it may print the
// credential.
func TestCheckResultNeverCarriesTheKey(t *testing.T) {
	cases := []map[string]int{
		{"/models": http.StatusOK},
		{"/models": http.StatusUnauthorized},
		{"/models": http.StatusForbidden, "/chat/completions": http.StatusNotFound},
		{"/models": http.StatusForbidden, "/chat/completions": http.StatusForbidden},
		{"/models": http.StatusTooManyRequests},
		{"/models": http.StatusTeapot},
	}
	for _, statuses := range cases {
		got := runProbe(t, newProbeRecorder(statuses), bearerAuth, "/chat/completions")
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal verdict: %v", err)
		}
		if strings.Contains(string(encoded), syntheticKey) || strings.Contains(got.line(), syntheticKey) {
			t.Fatalf("verdict for %v leaked the credential", statuses)
		}
	}
}

func TestSanitizeReasonRedactsTheKey(t *testing.T) {
	got := sanitizeReason(`Get "https://api.openai.com/v1/models?k=`+syntheticKey+`": dial error`, syntheticKey)
	if strings.Contains(got, syntheticKey) {
		t.Fatal("sanitizeReason left the credential in the reason")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitizeReason() = %q; want a redaction marker", got)
	}
}

func TestCheckResultLine(t *testing.T) {
	if got := (checkResult{Status: statusValid}).line(); got != "VALID" {
		t.Fatalf("line() = %q; want VALID", got)
	}
	if got := (checkResult{Status: statusUnknown, Reason: "rate limited"}).line(); got != "UNKNOWN (rate limited)" {
		t.Fatalf("line() = %q; want the reason inline", got)
	}
}

// TestProviderProbesConfigureCapability: every provider whose keys can be
// scope-restricted needs the escalation, or the false negative comes back for
// that provider alone.
func TestProviderProbesConfigureCapability(t *testing.T) {
	for name, probe := range providerProbes {
		if probe.capability == "" || probe.capabilityBody == "" {
			t.Fatalf("provider %q has no capability probe; a scope-restricted key would be undecidable", name)
		}
		if !strings.Contains(probe.capabilityBody, probeModel) {
			t.Fatalf("provider %q capability body does not use the non-billable sentinel model", name)
		}
	}
}
