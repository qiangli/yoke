package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppServiceRegistrationValidation(t *testing.T) {
	_, err := ParseRegistrationFile("/nonexistent.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	reg := DefaultFeedRegistration("http://127.0.0.1:18080", "as123", "hs456")
	if err := reg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	bad := reg
	bad.ID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for empty id")
	}
	bad = reg
	bad.Namespaces.Users[0].Regex = "[bad"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for bad regex")
	}
}

func TestAppServiceRegistrationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registration.yaml")
	reg := DefaultFeedRegistration("http://127.0.0.1:18080", "as-token-123", "hs-token-456")
	if err := WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	got, err := ParseRegistrationFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AsToken != reg.AsToken || got.HsToken != reg.HsToken {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, reg)
	}
	if got.Namespaces.Users[0].Regex != "@_feed_.*" {
		t.Fatalf("namespace regex %q", got.Namespaces.Users[0].Regex)
	}
	// File should be 0600
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %o", fi.Mode().Perm())
	}
}

func TestAppServiceTransactionHandlingWithFakeHomeserver(t *testing.T) {
	var received []Envelope
	reg := DefaultFeedRegistration("http://127.0.0.1:18080", "as-token-xyz", "hs-token-abc")
	app := NewAppService(reg, func(_ context.Context, e Envelope) error {
		received = append(received, e)
		return nil
	}, nil)

	// Fake homeserver: sends a transaction PUT to the appservice.
	// The appservice is the server; we simulate the homeserver's push.
	eventContent, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": "hello from matrix"})
	events := []MatrixEvent{
		{EventID: "$ev1", Sender: "@_feed_telegram_user42:example.com", Type: "m.room.message", RoomID: "!feed_telegram_chat1:example.com", Content: eventContent, OriginServerTS: time.Now().UnixMilli()},
	}
	body, _ := json.Marshal(map[string]any{"events": events})

	t.Run("valid hs_token query", func(t *testing.T) {
		received = nil
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1?access_token=hs-token-abc", bytes.NewReader(body))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if len(received) != 1 {
			t.Fatalf("received %d envelopes", len(received))
		}
		if received[0].Text != "hello from matrix" {
			t.Fatalf("text %q", received[0].Text)
		}
		// Virtual user @_feed_telegram_user42 -> telegram channel, conversation chat1
		if received[0].Channel != "telegram" {
			t.Fatalf("channel %q", received[0].Channel)
		}
		if received[0].ConversationID != "chat1" {
			t.Fatalf("conversation %q", received[0].ConversationID)
		}
	})

	t.Run("valid hs_token Bearer header", func(t *testing.T) {
		// Use a fresh app to avoid message dedupe from prior txn.
		var received2 []Envelope
		app2 := NewAppService(reg, func(_ context.Context, e Envelope) error {
			received2 = append(received2, e)
			return nil
		}, nil)
		body2, _ := json.Marshal(map[string]any{"events": []MatrixEvent{
			{EventID: "$ev2", Sender: "@alice:example.com", Type: "m.room.message", RoomID: "!feed_telegram_chat1:example.com", Content: eventContent, OriginServerTS: time.Now().UnixMilli()},
		}})
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn2", bytes.NewReader(body2))
		req.Header.Set("Authorization", "Bearer hs-token-abc")
		w := httptest.NewRecorder()
		app2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if len(received2) != 1 {
			t.Fatalf("received %d", len(received2))
		}
	})

	t.Run("bad token rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn-bad?access_token=wrong-token", bytes.NewReader(body))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 got %d", w.Code)
		}
	})

	t.Run("duplicate txn deduped", func(t *testing.T) {
		received = nil
		// txn1 was already seen above; re-send same txn id
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1?access_token=hs-token-abc", bytes.NewReader(body))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		if len(received) != 0 {
			t.Fatalf("duplicate txn should be empty, got %d", len(received))
		}
	})

	t.Run("dedupe by message_id within service", func(t *testing.T) {
		// New txn id but same event_id => deduped by origin key
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn-new?access_token=hs-token-abc", bytes.NewReader(body))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		if len(received) != 0 {
			t.Fatalf("duplicate message should be deduped, got %d", len(received))
		}
	})

	t.Run("wrong method rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_matrix/app/v1/transactions/txn-x?access_token=hs-token-abc", nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405 got %d", w.Code)
		}
	})

	t.Run("user query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_matrix/app/v1/users/@_feed_telegram_123:example.com?access_token=hs-token-abc", nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("user query status %d", w.Code)
		}
		req2 := httptest.NewRequest(http.MethodGet, "/_matrix/app/v1/users/@unknown:example.com?access_token=hs-token-abc", nil)
		w2 := httptest.NewRecorder()
		app.ServeHTTP(w2, req2)
		if w2.Code != http.StatusNotFound {
			t.Fatalf("expected 404 got %d", w2.Code)
		}
	})

	t.Run("room alias query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_matrix/app/v1/rooms/#feed_telegram_chat1:example.com?access_token=hs-token-abc", nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("room query status %d", w.Code)
		}
		req2 := httptest.NewRequest(http.MethodGet, "/_matrix/app/v1/rooms/#other:example.com?access_token=hs-token-abc", nil)
		w2 := httptest.NewRecorder()
		app.ServeHTTP(w2, req2)
		if w2.Code != http.StatusNotFound {
			t.Fatalf("expected 404 got %d", w2.Code)
		}
	})
}

func TestVirtualUsers(t *testing.T) {
	reg := DefaultFeedRegistration("http://example.com:8008", "as", "hs")
	app := NewAppService(reg, nil, nil)
	// Override server name for deterministic test
	app.serverName = "example.com"
	uid := app.VirtualUserID("12345", "telegram")
	if uid != "@_feed_telegram_12345:example.com" {
		t.Fatalf("virtual user %q", uid)
	}
	rid := app.BridgedRoomID("telegram", "chat1")
	if rid != "!feed_telegram_chat1:example.com" {
		t.Fatalf("room id %q", rid)
	}
	alias := app.BridgedRoomAlias("telegram", "chat1")
	if alias != "#feed_telegram_chat1:example.com" {
		t.Fatalf("alias %q", alias)
	}
}

func TestMatrixEventToEnvelope(t *testing.T) {
	reg := DefaultFeedRegistration("http://example.com:8008", "as", "hs")
	app := NewAppService(reg, nil, nil)
	app.serverName = "example.com"

	t.Run("bridged room extracted", func(t *testing.T) {
		content, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": "hi"})
		ev := MatrixEvent{EventID: "$ev1", Sender: "@_feed_telegram_user1:example.com", Type: "m.room.message", RoomID: "!feed_telegram_chat1:example.com", Content: content, OriginServerTS: time.Now().UnixMilli()}
		env, err := app.matrixEventToEnvelope(ev)
		if err != nil {
			t.Fatal(err)
		}
		if env.Channel != "telegram" || env.ConversationID != "chat1" {
			t.Fatalf("channel %q conv %q", env.Channel, env.ConversationID)
		}
		// Sender is virtual user -> telegram:user1
		if env.SenderID != "telegram:user1" {
			t.Fatalf("sender %q", env.SenderID)
		}
	})

	t.Run("native matrix room stays matrix", func(t *testing.T) {
		content, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": "matrix native"})
		ev := MatrixEvent{EventID: "$ev2", Sender: "@alice:example.com", Type: "m.room.message", RoomID: "!nativeRoom:example.com", Content: content, OriginServerTS: time.Now().UnixMilli()}
		env, err := app.matrixEventToEnvelope(ev)
		if err != nil {
			t.Fatal(err)
		}
		if env.Channel != "matrix" {
			t.Fatalf("channel %q", env.Channel)
		}
	})

	t.Run("non-text msgtype skipped", func(t *testing.T) {
		content, _ := json.Marshal(map[string]any{"msgtype": "m.image", "body": "image"})
		ev := MatrixEvent{EventID: "$ev3", Sender: "@alice:example.com", Type: "m.room.message", RoomID: "!native:example.com", Content: content, OriginServerTS: 0}
		_, err := app.matrixEventToEnvelope(ev)
		if err == nil {
			t.Fatal("expected error for m.image")
		}
	})
}
