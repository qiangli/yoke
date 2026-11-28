package broker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
)

func signedOfferRequest(method, path, app, secret string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	prefix := "/matrix/h/host/app/offer-" + app
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	user := "member@example.test"
	r.Header.Set(coopauth.HdrForwardedPrefix, prefix)
	r.Header.Set(coopauth.HdrRemoteUser, user)
	r.Header.Set(coopauth.HdrIdentityTs, ts)
	r.Header.Set(coopauth.HdrRelationship, coopauth.RelMember)
	sign := func(value string) string {
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(value))
		return hex.EncodeToString(h.Sum(nil))
	}
	r.Header.Set(coopauth.HdrIdentitySig, sign(user+"\n\n"+prefix+"\n"+ts))
	r.Header.Set(coopauth.HdrRelationshipSig, sign("v2\nmember\n\n\n\n"+user+"\n"+prefix+"\n"+ts))
	return r
}

func TestOfferDefinitionAudienceAndPeerAdmission(t *testing.T) {
	dir := t.TempDir()
	rec := OfferRecord{Name: "sample", Definition: json.RawMessage(`{"name":"sample","class":"tool","unit":"call"}`), Digest: "abc", SSOSecret: "test-secret"}
	if err := SaveOffer(dir, rec); err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), Options{Token: "owner-key", OffersDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"signed member", signedOfferRequest("GET", "/offers/sample?definition", "sample", rec.SSOSecret), 200},
		{"wrong app", signedOfferRequest("GET", "/offers/sample?definition", "other", rec.SSOSecret), 403},
		{"wrong signature", signedOfferRequest("GET", "/offers/sample?definition", "sample", "wrong"), 403},
		{"another offer", signedOfferRequest("GET", "/offers/other?definition", "sample", rec.SSOSecret), 403},
		{"unsigned", httptest.NewRequest("GET", "/offers/sample?definition", nil), 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			b.ServeHTTP(w, tc.req)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 200 {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["digest"] != "abc" {
					t.Fatalf("missing digest: %s", w.Body.String())
				}
				if _, ok := body["sso_secret"]; ok {
					t.Fatal("secret leaked")
				}
			}
		})
	}
	if peerOfferAllowed("GET", "/offers/sample", []string{"other"}) || !peerOfferAllowed("POST", "/offers/sample", []string{"sample"}) {
		t.Fatal("peer offer allow-list")
	}
	if !peerFilesOfferAllowed("GET", "/files/docs/sub/file.txt", []string{"files-docs"}) || peerFilesOfferAllowed("GET", "/files/other", []string{"files-docs"}) {
		t.Fatal("peer files offer allow-list")
	}
}

func TestOfferClassInferenceAdmission(t *testing.T) {
	dir := t.TempDir()
	for _, class := range []string{"tool", "model", "agent"} {
		rec := OfferRecord{Name: class, Definition: json.RawMessage(`{"name":"` + class + `","class":"` + class + `","unit":"token"}`), SSOSecret: "secret"}
		if err := SaveOffer(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	b, err := New(t.Context(), Options{Token: "owner-key", OffersDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		offer, model string
		want         bool
	}{{"tool", "tool", false}, {"agent", "agent", false}, {"model", "other", false}, {"model", "model", true}} {
		w := httptest.NewRecorder()
		body := []byte(`{"model":"` + tc.model + `"}`)
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		if got := b.memberOfferRouteAllowed(w, r, r.URL.Path, tc.offer); got != tc.want {
			t.Errorf("%s/%s admitted=%v want %v", tc.offer, tc.model, got, tc.want)
		}
	}
}

func TestOfferLimitCountsTokenUnit(t *testing.T) {
	dir := t.TempDir()
	rec := OfferRecord{Name: "model", Definition: json.RawMessage(`{"name":"model","class":"model","unit":"token"}`), SSOSecret: "secret", Limit: 5}
	if err := SaveOffer(dir, rec); err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), Options{Token: "owner-key", OffersDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	offerRunner.Lock()
	old := offerRunner.run
	offerRunner.run = func(_ context.Context, _ OfferRecord, _ map[string]any) (json.RawMessage, any, error) {
		return json.RawMessage(`{"ok":true}`), map[string]int{"input": 3, "output": 2}, nil
	}
	offerRunner.Unlock()
	t.Cleanup(func() { offerRunner.Lock(); offerRunner.run = old; offerRunner.Unlock() })
	for i, want := range []int{200, 429} {
		r := signedOfferRequest("POST", "/offers/model", "model", rec.SSOSecret)
		r.Body = io.NopCloser(bytes.NewReader([]byte(`{"args":{}}`)))
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("call %d status %d, want %d: %s", i, w.Code, want, w.Body.String())
		}
	}
}

func TestOfferCallRefusesStalePinnedDigest(t *testing.T) {
	dir := t.TempDir()
	rec := OfferRecord{Name: "sample", Definition: json.RawMessage(`{"name":"sample","class":"tool","unit":"call"}`), Digest: "current", SSOSecret: "secret"}
	if err := SaveOffer(dir, rec); err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), Options{Token: "owner-key", OffersDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	offerRunner.Lock()
	old := offerRunner.run
	offerRunner.run = func(_ context.Context, _ OfferRecord, _ map[string]any) (json.RawMessage, any, error) {
		ran++
		return json.RawMessage(`{"ok":true}`), map[string]int{"calls": 1}, nil
	}
	offerRunner.Unlock()
	t.Cleanup(func() { offerRunner.Lock(); offerRunner.run = old; offerRunner.Unlock() })
	for _, tc := range []struct {
		pinned string
		want   int
	}{{"stale", 409}, {"current", 200}, {"", 200}} {
		r := signedOfferRequest("POST", "/offers/sample", "sample", rec.SSOSecret)
		r.Body = io.NopCloser(bytes.NewReader([]byte(`{"args":{}}`)))
		if tc.pinned != "" {
			r.Header.Set(OfferDigestHeader, tc.pinned)
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("pinned %q status %d, want %d: %s", tc.pinned, w.Code, tc.want, w.Body.String())
		}
		if tc.want == 409 && !bytes.Contains(w.Body.Bytes(), []byte("bashy remote add HOST sample")) {
			t.Fatalf("refusal does not name the fix: %s", w.Body.String())
		}
	}
	if ran != 2 {
		t.Fatalf("runner ran %d times, want 2", ran)
	}
}
