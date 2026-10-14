package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/feed"
)

func TestDiscordSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing auth")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "discord-msg-1"})
	}))
	defer srv.Close()

	a := &DiscordAdapter{Token: "bot-token", BaseURL: srv.URL, Client: srv.Client()}
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "discord", MessageID: "m1", ConversationID: "123456", SenderID: "s1", Text: "hello discord", ReceivedAt: time.Now()}
	ids, err := a.Send(context.Background(), []feed.Envelope{env})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != "discord-msg-1" {
		t.Fatalf("id %q", ids[0])
	}
}

func TestDiscordHandleDeliveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok"})
	}))
	defer srv.Close()

	a := &DiscordAdapter{Token: "bot-token", BaseURL: srv.URL, Client: srv.Client()}
	req := feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{
		{Version: feed.ContractVersion, Channel: "discord", MessageID: "m1", ConversationID: "123", SenderID: "s1", Text: "via stdin", ReceivedAt: time.Now()},
	}}
	body, _ := json.Marshal(req)
	var out bytes.Buffer
	if err := a.HandleDeliveryRequest(context.Background(), bytes.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	var res feed.DeliveryResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.DeliveredIDs) != 1 {
		t.Fatalf("delivered %v", res.DeliveredIDs)
	}
}
