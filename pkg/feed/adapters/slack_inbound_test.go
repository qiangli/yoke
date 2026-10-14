package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/feed"
)

func TestSlackSocketInboundAckReconnectPairingPhotoApprove(t *testing.T) {
	var mu sync.Mutex
	var acks []string
	var opens int
	var photoGets int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/apps.connections.open":
			if r.Header.Get("Authorization") != "Bearer app-secret" {
				t.Errorf("app token header missing")
			}
			mu.Lock()
			opens++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http") + "/socket?ticket=secret"})
		case "/photo":
			if r.Header.Get("Authorization") != "Bearer bot-secret" {
				t.Errorf("photo bot token header missing")
			}
			mu.Lock()
			photoGets++
			mu.Unlock()
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image"))
		case "/socket":
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Logf("upgrade: %v", err)
				return
			}
			defer c.Close()
			mu.Lock()
			n := opens
			mu.Unlock()
			message := func(id, user, body string, photo bool) {
				e := map[string]any{"type": "message", "user": user, "channel": "C1", "channel_type": "channel", "ts": id, "text": body}
				if photo {
					e["files"] = []map[string]string{{"name": "p.png", "mimetype": "image/png", "url_private_download": server.URL + "/photo"}}
				}
				p := map[string]any{"event_id": "ev-" + id, "event": e}
				if err := c.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "ack-" + id, "payload": p}); err != nil {
					t.Logf("send: %v", err)
				}
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				if err := c.ReadJSON(&ack); err == nil {
					mu.Lock()
					acks = append(acks, ack.EnvelopeID)
					mu.Unlock()
				} else {
					t.Logf("ack read: %v", err)
				}
			}
			if n == 1 {
				message("1.000", "stranger", "/approve bad", true)
				message("2.000", "alice", "/approve draft-1", false)
				message("3.000", "alice", "look", true)
				_ = c.WriteJSON(map[string]any{"type": "disconnect", "reason": "refresh_requested"})
			} else {
				message("3.000", "alice", "look", true)
				<-r.Context().Done()
			}
		}
	}))
	defer server.Close()
	a := &SlackAdapter{Token: "bot-secret", AppToken: "app-secret", BaseURL: server.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := make(chan feed.Envelope, 3)
	done := make(chan error, 1)
	go func() {
		done <- a.Listen(ctx, func(_ context.Context, e feed.Envelope) error { got <- e; return nil }, pairedUser("alice"))
	}()
	var envs []feed.Envelope
	for len(envs) < 2 {
		select {
		case e := <-got:
			envs = append(envs, e)
		case <-ctx.Done():
			mu.Lock()
			t.Logf("opens=%d acks=%v photos=%d", opens, acks, photoGets)
			mu.Unlock()
			select {
			case err := <-done:
				t.Logf("listen error: %v", err)
			default:
			}
			t.Fatal("timed out waiting for messages")
		}
	}
	if envs[0].Text != "/approve draft-1" || len(envs[1].Attachments) != 1 {
		t.Fatalf("envelopes: %+v", envs)
	}
	t.Cleanup(func() { _ = os.Remove(envs[1].Attachments[0].Path) })
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		ready := len(acks) >= 4
		mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("missing acks/reconnect")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case e := <-got:
		t.Fatalf("duplicate/unpaired forwarded: %+v", e)
	case <-time.After(20 * time.Millisecond):
	}
	mu.Lock()
	gotAcks := append([]string(nil), acks...)
	gotOpens, gotPhotos := opens, photoGets
	mu.Unlock()
	if gotOpens < 2 || gotPhotos != 1 || strings.Join(gotAcks, ",") != "ack-1.000,ack-2.000,ack-3.000,ack-3.000" {
		t.Fatalf("opens=%d photos=%d acks=%v", gotOpens, gotPhotos, gotAcks)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestSlackSocketRetriesUnacknowledgedHandlerFailure(t *testing.T) {
	var attempts, connections int
	var mu sync.Mutex
	acked := make(chan string, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/apps.connections.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(server.URL, "http") + "/socket"})
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		mu.Lock()
		connections++
		mu.Unlock()
		_ = c.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "ack-retry", "payload": map[string]any{"event_id": "same", "event": map[string]string{"type": "message", "user": "alice", "channel": "C1", "ts": "1.0", "text": "retry"}}})
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if c.ReadJSON(&ack) == nil {
			acked <- ack.EnvelopeID
		}
	}))
	defer server.Close()
	a := &SlackAdapter{Token: "bot", AppToken: "app", BaseURL: server.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.Listen(ctx, func(context.Context, feed.Envelope) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n == 1 {
				return errors.New("temporary failure")
			}
			return nil
		}, pairedUser("alice"))
	}()
	select {
	case id := <-acked:
		if id != "ack-retry" {
			t.Fatalf("ack=%q", id)
		}
	case <-ctx.Done():
		t.Fatal("event not retried and acknowledged")
	}
	mu.Lock()
	gotAttempts, gotConnections := attempts, connections
	mu.Unlock()
	if gotAttempts != 2 || gotConnections < 2 {
		t.Fatalf("attempts=%d connections=%d", gotAttempts, gotConnections)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}
