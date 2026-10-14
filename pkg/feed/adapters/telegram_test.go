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

func TestTelegramSend(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 42}})
	}))
	defer srv.Close()

	a := &TelegramAdapter{Token: "test-token", BaseURL: srv.URL, Client: srv.Client()}
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "123", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
	ids, err := a.Send(context.Background(), []feed.Envelope{env})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "42" {
		t.Fatalf("ids %v", ids)
	}
	if gotBody["text"] != "hello" {
		t.Fatalf("body text %q", gotBody["text"])
	}
}

func TestTelegramPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{{
				"update_id": 100,
				"message": map[string]any{
					"message_id": 1,
					"chat":       map[string]any{"id": 123, "type": "private"},
					"from":       map[string]any{"id": 456, "username": "alice"},
					"text":       "hi from telegram",
					"date":       time.Now().Unix(),
				},
			}},
		})
	}))
	defer srv.Close()

	a := &TelegramAdapter{Token: "test", BaseURL: srv.URL, Client: srv.Client()}
	envs, next, err := a.Poll(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != 101 {
		t.Fatalf("next %d", next)
	}
	if len(envs) != 1 || envs[0].Text != "hi from telegram" || !envs[0].IsDM {
		t.Fatalf("envs %+v", envs)
	}
}

func TestTelegramHandleDeliveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 99}})
	}))
	defer srv.Close()

	a := &TelegramAdapter{Token: "test", BaseURL: srv.URL, Client: srv.Client()}
	req := feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{
		{Version: feed.ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "123", SenderID: "s1", Text: "via stdin", ReceivedAt: time.Now()},
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
