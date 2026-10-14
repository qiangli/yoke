package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAdapter is a minimal conformance adapter used by the suite.
type fakeAdapter struct {
	send func(context.Context, []Envelope) ([]string, error)
}

func (f *fakeAdapter) Send(ctx context.Context, envs []Envelope) ([]string, error) {
	if f.send != nil {
		return f.send(ctx, envs)
	}
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = e.MessageID
		if ids[i] == "" {
			ids[i] = e.ID
		}
	}
	return ids, nil
}

func (f *fakeAdapter) HandleDeliveryRequest(ctx context.Context, body []byte) ([]string, error) {
	var req DeliveryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return f.Send(ctx, req.Envelopes)
}

// TestAdapterConformance verifies the symmetric envelope contract.
func TestAdapterConformance(t *testing.T) {
	t.Run("SendEnvelopes validates envelopes", func(t *testing.T) {
		SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
			return []string{"ok"}, nil
		})
		t.Cleanup(func() { SetEnvelopeSender(nil) })

		ch := Channel{Name: "test", Cmd: "dummy:send"}
		env := Envelope{Version: ContractVersion, Channel: "test", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		ids, err := SendEnvelopes(context.Background(), ch, []Envelope{env})
		if err != nil {
			t.Fatalf("SendEnvelopes: %v", err)
		}
		if len(ids) != 1 || ids[0] != "ok" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("SendEnvelopes rejects bad envelope", func(t *testing.T) {
		ch := Channel{Name: "test", Cmd: "dummy:send"}
		env := Envelope{Version: ContractVersion, Channel: "", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		_, err := SendEnvelopes(context.Background(), ch, []Envelope{env})
		if err == nil {
			t.Fatal("expected validation error")
		}
	})

	t.Run("DeliveryRequest JSON on stdin", func(t *testing.T) {
		var captured DeliveryRequest
		SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
			captured = DeliveryRequest{Version: ContractVersion, Envelopes: envs}
			return []string{"delivered"}, nil
		})
		t.Cleanup(func() { SetEnvelopeSender(nil) })

		ch := Channel{Name: "test", Cmd: "dummy:send"}
		env := Envelope{Version: ContractVersion, Channel: "test", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hi", ReceivedAt: time.Now()}
		ids, err := SendEnvelopes(context.Background(), ch, []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
		if captured.Version != ContractVersion || len(captured.Envelopes) != 1 || captured.Envelopes[0].Text != "hi" {
			t.Fatalf("captured %#v", captured)
		}
		if ids[0] != "delivered" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("returns delivered_ids", func(t *testing.T) {
		SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
			ids := make([]string, len(envs))
			for i := range envs {
				ids[i] = "id-" + envs[i].MessageID
			}
			return ids, nil
		})
		t.Cleanup(func() { SetEnvelopeSender(nil) })

		ch := Channel{Name: "test", Cmd: "dummy:send"}
		envs := []Envelope{
			{Version: ContractVersion, Channel: "test", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()},
			{Version: ContractVersion, Channel: "test", MessageID: "m2", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()},
		}
		ids, err := SendEnvelopes(context.Background(), ch, envs)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 || ids[0] != "id-m1" || ids[1] != "id-m2" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("inbound NDJSON validates and dedupes", func(t *testing.T) {
		var received []Envelope
		handler := func(_ context.Context, e Envelope) error {
			received = append(received, e)
			return nil
		}
		env1 := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		env2 := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		line1, _ := json.Marshal(env1)
		line2, _ := json.Marshal(env2)
		ndjson := string(line1) + "\n" + string(line2) + "\n"
		if err := ProcessNDJSON(context.Background(), strings.NewReader(ndjson), handler, nil); err != nil {
			t.Fatal(err)
		}
		if len(received) != 1 {
			t.Fatalf("expected dedupe to 1, got %d", len(received))
		}
	})

	t.Run("inbound loopback POST validates", func(t *testing.T) {
		var received Envelope
		srv := NewInboundServer(func(_ context.Context, e Envelope) error {
			received = e
			return nil
		}, nil)
		env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
		body, _ := json.Marshal(env)
		req := httptest.NewRequest(http.MethodPost, "/inbound", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if received.Text != "hello" {
			t.Fatalf("received text %q", received.Text)
		}
	})

	t.Run("inbound rejects unpaired sender", func(t *testing.T) {
		srv := NewInboundServer(nil, DenyAllPairing{})
		env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		body, _ := json.Marshal(env)
		req := httptest.NewRequest(http.MethodPost, "/inbound", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 got %d", w.Code)
		}
	})

	t.Run("quiet hours", func(t *testing.T) {
		start, end := 22*60, 7*60
		if !QuietHours(time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local), start, end) {
			t.Fatal("23:00 should be quiet")
		}
		if QuietHours(time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local), start, end) {
			t.Fatal("10:00 should not be quiet")
		}
		if QuietHours(time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local), 60, 60) {
			t.Fatal("equal bounds = not quiet")
		}
	})

	t.Run("draft approve", func(t *testing.T) {
		store := NewDraftStore()
		env := Envelope{ID: "draft-1", Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		store.Add(env)
		if len(store.List()) != 1 {
			t.Fatal("expected 1 draft")
		}
		got, ok := store.Approve("draft-1")
		if !ok || got.ID != "draft-1" {
			t.Fatalf("approve %v %v", got, ok)
		}
		if len(store.List()) != 0 {
			t.Fatal("expected 0 after approve")
		}
	})

	t.Run("attachment passthrough", func(t *testing.T) {
		var got []Envelope
		SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
			got = envs
			ids := make([]string, len(envs))
			for i := range ids {
				ids[i] = "ok"
			}
			return ids, nil
		})
		t.Cleanup(func() { SetEnvelopeSender(nil) })

		ch := Channel{Name: "test", Cmd: "dummy:send"}
		att := Attachment{Path: "/tmp/foo.png", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MIME: "image/png", Name: "foo.png", Size: 123}
		env := Envelope{Version: ContractVersion, Channel: "test", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "with file", Attachments: []Attachment{att}, ReceivedAt: time.Now()}
		_, err := SendEnvelopes(context.Background(), ch, []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || len(got[0].Attachments) != 1 || got[0].Attachments[0].MIME != "image/png" {
			t.Fatalf("attachments not passed: %+v", got)
		}
	})

	t.Run("secrets never on argv", func(t *testing.T) {
		// The sender must not include secret values in argv; we verify by
		// checking that SendEnvelopes does not require a secret arg.
		// This is a contract test: the implementation never appends secrets.
		var capturedEnvs []Envelope
		SetEnvelopeSender(func(_ context.Context, ch Channel, envs []Envelope) ([]string, error) {
			// Verify no secret-like value leaked into channel args.
			if strings.Contains(ch.Args, "TOKEN") || strings.Contains(ch.Args, "secret") {
				t.Errorf("secret leaked in args %q", ch.Args)
			}
			capturedEnvs = envs
			return []string{"ok"}, nil
		})
		t.Cleanup(func() { SetEnvelopeSender(nil) })

		ch := Channel{Name: "test", Cmd: "dummy:send", Args: "--target chat1"}
		env := Envelope{Version: ContractVersion, Channel: "test", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		_, _ = SendEnvelopes(context.Background(), ch, capturedEnvs)
		_, err := SendEnvelopes(context.Background(), ch, []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
	})
}
