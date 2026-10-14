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

func TestSlackSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer srv.Close()

	a := &SlackAdapter{Token: "xoxb-token", BaseURL: srv.URL, Client: srv.Client()}
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "slack", MessageID: "m1", ConversationID: "C123", SenderID: "U123", Text: "hello slack", ReceivedAt: time.Now()}
	ids, err := a.Send(context.Background(), []feed.Envelope{env})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != "1234.5678" {
		t.Fatalf("id %q", ids[0])
	}
}

func TestSlackHandleDeliveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "ok"})
	}))
	defer srv.Close()

	a := &SlackAdapter{Token: "xoxb", BaseURL: srv.URL, Client: srv.Client()}
	req := feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{
		{Version: feed.ContractVersion, Channel: "slack", MessageID: "m1", ConversationID: "C123", SenderID: "s1", Text: "via stdin", ReceivedAt: time.Now()},
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
