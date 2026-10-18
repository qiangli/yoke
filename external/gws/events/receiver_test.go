package events

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// push builds a Pub/Sub push request (recorded envelope shape).
func push(t *testing.T, url, msgID string, attrs, data string, hdr map[string]string) *http.Request {
	t.Helper()
	body := `{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(data)) + `","messageId":"` + msgID + `"` + attrs + `},"subscription":"projects/p/subscriptions/s"}`
	r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func serve(rv *Receiver, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	rv.ServeHTTP(w, r)
	return w
}

func TestReceiverAuthentication(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	handled := 0
	rv := &Receiver{Auth: SharedToken(canary), Store: s, Handler: func(context.Context, Event) error { handled++; return nil }}
	gmail := `{"emailAddress":"test@example.invalid","historyId":"9001"}`
	for _, r := range []*http.Request{
		push(t, "/push", "m1", "", gmail, nil),
		push(t, "/push?token=wrong-"+canary[:4], "m1", "", gmail, nil),
		push(t, "/push", "m1", "", gmail, map[string]string{"X-Goog-Channel-Token": "nope"}),
	} {
		w := serve(rv, r)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), canary) {
			t.Fatalf("unauthenticated push: %d %q", w.Code, w.Body.String())
		}
	}
	if handled != 0 || s.Seen("m1") {
		t.Fatal("unauthenticated push must not be processed or acknowledged")
	}
	if w := serve(rv, push(t, "/push?token="+canary, "m1", "", gmail, nil)); w.Code != http.StatusNoContent || strings.Contains(w.Body.String(), canary) {
		t.Fatalf("authenticated push: %d", w.Code)
	}
	if w := serve(rv, push(t, "/push", "m2", "", gmail, map[string]string{"X-Goog-Channel-Token": canary})); w.Code != http.StatusNoContent {
		t.Fatalf("channel token push: %d", w.Code)
	}
	if handled != 2 {
		t.Fatalf("handled = %d", handled)
	}
	if w := serve(&Receiver{Auth: SharedToken(""), Store: s, Handler: rv.Handler}, push(t, "/push?token=", "m3", "", gmail, nil)); w.Code != http.StatusUnauthorized {
		t.Fatal("an empty secret must authenticate nothing")
	}
	if w := serve(&Receiver{Store: s, Handler: rv.Handler}, push(t, "/push", "m3", "", gmail, nil)); w.Code != http.StatusUnauthorized {
		t.Fatal("no authenticator must authenticate nothing")
	}
	get := httptest.NewRequest(http.MethodGet, "/push?token="+canary, nil)
	if w := serve(rv, get); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("only POST is accepted")
	}
}

func TestReceiverBearerVerifier(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	verify := func(ctx context.Context, jwt string) error {
		if jwt != "good.jwt.sig" {
			return errors.New("bad signature")
		}
		return nil
	}
	rv := &Receiver{Auth: BearerVerifier(verify), Store: s, Handler: func(context.Context, Event) error { return nil }}
	if w := serve(rv, push(t, "/p", "m1", "", `{}`, map[string]string{"Authorization": "Bearer forged." + canary})); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), canary) {
		t.Fatalf("forged bearer: %d %q", w.Code, w.Body.String())
	}
	if w := serve(rv, push(t, "/p", "m1", "", `{}`, map[string]string{"Authorization": "Bearer good.jwt.sig"})); w.Code != http.StatusNoContent {
		t.Fatalf("verified bearer: %d", w.Code)
	}
	if w := serve(&Receiver{Auth: BearerVerifier(nil), Store: s, Handler: rv.Handler}, push(t, "/p", "m2", "", `{}`, map[string]string{"Authorization": "Bearer good.jwt.sig"})); w.Code != http.StatusUnauthorized {
		t.Fatal("no verifier must authenticate nothing")
	}
}

func TestReceiverDuplicateDeliveryAndAckOrdering(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	var got []string
	fail := true
	rv := &Receiver{Auth: SharedToken(canary), Store: s, Handler: func(ctx context.Context, e Event) error {
		if fail {
			return errors.New("downstream unavailable")
		}
		got = append(got, e.ID+":"+e.Kind)
		return nil
	}}
	url := "/push?token=" + canary
	ce := `,"attributes":{"ce-id":"evt-1","ce-type":"google.workspace.chat.message.v1.created"}`
	// Handler failure → non-2xx and NOT acknowledged, so Pub/Sub redelivers.
	if w := serve(rv, push(t, url, "m1", ce, `{"message":{}}`, nil)); w.Code != http.StatusServiceUnavailable || s.Seen("evt-1") {
		t.Fatalf("failed handler: %d", w.Code)
	}
	fail = false
	// Redelivery under a NEW Pub/Sub messageId: the CloudEvents id dedups.
	for _, mid := range []string{"m2", "m3"} {
		if w := serve(rv, push(t, url, mid, ce, `{"message":{}}`, nil)); w.Code != http.StatusNoContent {
			t.Fatalf("redelivery %s: %d", mid, w.Code)
		}
	}
	// Gmail notification redelivered with the same messageId.
	for i := 0; i < 2; i++ {
		serve(rv, push(t, url, "g1", "", `{"emailAddress":"a@example.invalid","historyId":"5"}`, nil))
	}
	if strings.Join(got, ",") != "evt-1:google.workspace.chat.message.v1.created,g1:pubsub.push" {
		t.Fatalf("handled = %v", got)
	}
	if !s.Seen("evt-1") || !s.Seen("g1") {
		t.Fatal("processed events must be acknowledged in the checkpoint")
	}
}

func TestReceiverBoundsAndMalformedInput(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	rv := &Receiver{Auth: SharedToken(canary), Store: s, Handler: func(context.Context, Event) error { return nil }}
	url := "/push?token=" + canary
	big := httptest.NewRequest(http.MethodPost, url, strings.NewReader(strings.Repeat("x", MaxPushBody+1)))
	if w := serve(rv, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", w.Code)
	}
	for _, body := range []string{`not json`, `{"message":{"data":"e30="}}`, `{"message":{"messageId":"m","data":"!!"}}`} {
		r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if w := serve(rv, r); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
}
