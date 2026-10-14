package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/feed"
)

type pairedUser string

func (p pairedUser) IsPaired(id string) bool { return id == string(p) }

func TestDiscordGatewayInboundResumePairingPhotoApprove(t *testing.T) {
	var connections atomic.Int32
	var resumeSeen atomic.Bool
	var photoGets atomic.Int32
	duplicateProcessed := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/photo" {
			photoGets.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image"))
			return
		}
		if r.URL.Path == "/api/v10/gateway/bot" {
			if r.Header.Get("Authorization") != "Bot secret" {
				t.Errorf("bot token header missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway"})
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := connections.Add(1)
		_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		var auth struct {
			Op int            `json:"op"`
			D  map[string]any `json:"d"`
		}
		if c.ReadJSON(&auth) != nil {
			return
		}
		if n == 1 {
			if auth.Op != 2 {
				t.Errorf("first opcode = %d", auth.Op)
			}
			_ = c.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]string{"session_id": "session-1", "resume_gateway_url": "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway"}})
		} else {
			if auth.Op == 6 && auth.D["session_id"] == "session-1" && auth.D["seq"] == float64(4) {
				resumeSeen.Store(true)
			}
		}
		message := func(id, sender, body string, seq int, photo bool) {
			d := map[string]any{"id": id, "channel_id": "room", "guild_id": "guild", "content": body, "timestamp": "2026-10-14T00:00:00Z", "author": map[string]any{"id": sender, "username": sender}}
			if photo {
				d["attachments"] = []map[string]string{{"filename": "p.png", "content_type": "image/png", "url": server.URL + "/photo"}}
			}
			_ = c.WriteJSON(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": seq, "d": d})
		}
		if n == 1 {
			message("unpaired", "stranger", "/approve bad", 2, true)
			message("approved", "alice", "/approve draft-1", 3, false)
			message("photo", "alice", "look", 4, true)
			_ = c.WriteJSON(map[string]any{"op": 7, "d": nil})
		} else {
			message("photo", "alice", "look", 5, true) // duplicate after resume
			_ = c.WriteJSON(map[string]any{"op": 1, "d": nil})
			var heartbeat struct {
				Op int `json:"op"`
			}
			if c.ReadJSON(&heartbeat) == nil && heartbeat.Op == 1 {
				close(duplicateProcessed)
			}
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	a := &DiscordAdapter{Token: "secret", BaseURL: server.URL}
	dir := t.TempDir()
	if err := feed.SaveBridges(dir, []feed.BridgeRoute{{ID: "discord:room->matrix:!room:example", SrcChannel: "discord", SrcConversation: "room", DstChannel: "matrix", DstConversation: "!room:example", CreatedAt: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	bridged := make(chan feed.Envelope, 3)
	feed.SetEnvelopeSender(func(_ context.Context, _ feed.Channel, envs []feed.Envelope) ([]string, error) {
		for _, e := range envs {
			bridged <- e
		}
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { feed.SetEnvelopeSender(nil) })
	engine := feed.NewBridgeEngine(dir, func() ([]feed.Channel, error) { return []feed.Channel{{Name: "matrix", Cmd: "dummy:send"}}, nil }, pairedUser("alice"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := make(chan feed.Envelope, 3)
	done := make(chan error, 1)
	go func() {
		done <- a.Listen(ctx, func(ctx context.Context, e feed.Envelope) error { got <- e; return engine.Handle(ctx, e) }, pairedUser("alice"))
	}()
	var envs []feed.Envelope
	for len(envs) < 2 {
		select {
		case e := <-got:
			envs = append(envs, e)
		case <-ctx.Done():
			t.Fatal("timed out waiting for messages")
		}
	}
	if envs[0].Text != "/approve draft-1" || envs[1].Text != "look" || len(envs[1].Attachments) != 1 {
		t.Fatalf("envelopes: %+v", envs)
	}
	t.Cleanup(func() { _ = os.Remove(envs[1].Attachments[0].Path) })
	for _, want := range []string{"/approve draft-1", "look"} {
		select {
		case e := <-bridged:
			if e.Channel != "matrix" || !strings.Contains(e.Text, want) {
				t.Fatalf("bridged: %+v", e)
			}
			if want == "look" && len(e.Attachments) != 1 {
				t.Fatalf("bridged photo missing: %+v", e)
			}
		case <-ctx.Done():
			t.Fatal("bridge did not receive message")
		}
	}
	deadline := time.After(time.Second)
	for !resumeSeen.Load() {
		select {
		case <-deadline:
			t.Fatal("no resume")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-duplicateProcessed:
	case <-ctx.Done():
		t.Fatal("duplicate dispatch not processed")
	}
	select {
	case extra := <-got:
		t.Fatalf("duplicate/unpaired forwarded: %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
	if photoGets.Load() != 1 {
		t.Fatalf("photo fetches=%d", photoGets.Load())
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDiscordGatewayInvalidSessionAndHeartbeat(t *testing.T) {
	var connections atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := connections.Add(1)
		_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 20}})
		var auth struct {
			Op int `json:"op"`
		}
		if c.ReadJSON(&auth) != nil {
			return
		}
		if auth.Op != 2 {
			t.Errorf("identify opcode=%d", auth.Op)
		}
		if n == 1 {
			_ = c.WriteJSON(map[string]any{"op": 9, "d": false})
			return
		}
		var heartbeat struct {
			Op int `json:"op"`
		}
		if c.ReadJSON(&heartbeat) == nil && heartbeat.Op == 1 {
			close(ready)
			_ = c.WriteJSON(map[string]any{"op": 11, "d": nil})
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	a := &DiscordAdapter{Token: "secret", GatewayURL: "ws" + strings.TrimPrefix(server.URL, "http")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.Listen(ctx, func(context.Context, feed.Envelope) error { return nil }, pairedUser("alice"))
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("no identify after invalid session or heartbeat")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}
