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

func TestMatrixSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$ev123"})
	}))
	defer srv.Close()

	a := &MatrixAdapter{Token: "tok", BaseURL: srv.URL, Client: srv.Client()}
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "matrix", MessageID: "m1", ConversationID: "!room:example.com", SenderID: "@alice:example.com", Text: "hello matrix", ReceivedAt: time.Now()}
	ids, err := a.Send(context.Background(), []feed.Envelope{env})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != "$ev123" {
		t.Fatalf("id %q", ids[0])
	}
}

func TestMatrixSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next_batch": "s2",
			"rooms": map[string]any{
				"join": map[string]any{
					"!room:example.com": map[string]any{
						"timeline": map[string]any{
							"events": []map[string]any{{
								"event_id":         "$ev1",
								"sender":           "@alice:example.com",
								"origin_server_ts": time.Now().UnixMilli(),
								"content":          map[string]any{"body": "matrix hi", "msgtype": "m.text"},
							}},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	a := &MatrixAdapter{Token: "tok", BaseURL: srv.URL, Client: srv.Client()}
	envs, next, err := a.Sync(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if next != "s2" {
		t.Fatalf("next %q", next)
	}
	if len(envs) != 1 || envs[0].Text != "matrix hi" {
		t.Fatalf("envs %+v", envs)
	}
}

func TestMatrixHandleDeliveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$ok"})
	}))
	defer srv.Close()

	a := &MatrixAdapter{Token: "tok", BaseURL: srv.URL, Client: srv.Client()}
	req := feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{
		{Version: feed.ContractVersion, Channel: "matrix", MessageID: "m1", ConversationID: "!room:example.com", SenderID: "s1", Text: "via stdin", ReceivedAt: time.Now()},
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
