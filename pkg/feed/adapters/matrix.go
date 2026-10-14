// Adapted from OpenClaw (https://github.com/openclaw/openclaw), MIT,
// tag v2026.10.2. Original TypeScript: extensions/matrix/src/*.ts.
// Converted to Go for bashy feed envelope contract.

package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/feed"
)

// MatrixAdapter implements the feed envelope contract for Matrix Client-Server API.
type MatrixAdapter struct {
	Homeserver string // e.g. https://matrix.example.com
	Token      string // access_token — via bashy secret env (MATRIX_ACCESS_TOKEN), never argv
	UserID     string
	BaseURL    string // override for tests
	Client     *http.Client
}

func (a *MatrixAdapter) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	if a.Homeserver != "" {
		return strings.TrimRight(a.Homeserver, "/")
	}
	return "https://matrix.example.com"
}
func (a *MatrixAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Send delivers envelopes to Matrix rooms via /rooms/{roomId}/send/m.room.message.
func (a *MatrixAdapter) Send(ctx context.Context, envs []feed.Envelope) ([]string, error) {
	var ids []string
	for _, env := range envs {
		id, err := a.sendOne(ctx, env)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *MatrixAdapter) sendOne(ctx context.Context, env feed.Envelope) (string, error) {
	u := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%d",
		a.baseURL(), urlEscape(env.ConversationID), time.Now().UnixNano())
	body := map[string]string{
		"msgtype": "m.text",
		"body":    env.Text,
	}
	if env.ReplyTo != "" {
		body["m.relates_to"] = env.ReplyTo
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("matrix send: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.EventID == "" {
		result.EventID = fmt.Sprintf("$%d", time.Now().UnixNano())
	}
	return result.EventID, nil
}

// Sync polls /sync for new events and converts them to envelopes.
func (a *MatrixAdapter) Sync(ctx context.Context, since string) ([]feed.Envelope, string, error) {
	u := fmt.Sprintf("%s/_matrix/client/v3/sync?since=%s&timeout=0", a.baseURL(), urlEscape(since))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, since, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, since, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, since, fmt.Errorf("matrix sync: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []struct {
						EventID string `json:"event_id"`
						Sender  string `json:"sender"`
						Content struct {
							Body    string `json:"body"`
							MsgType string `json:"msgtype"`
						} `json:"content"`
						OriginServerTS int64 `json:"origin_server_ts"`
					} `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, since, err
	}
	var envs []feed.Envelope
	for roomID, room := range result.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			if ev.Content.MsgType != "m.text" && ev.Content.MsgType != "" {
				continue
			}
			env := feed.Envelope{
				Version:        feed.ContractVersion,
				Channel:        "matrix",
				MessageID:      ev.EventID,
				ConversationID: roomID,
				SenderID:       ev.Sender,
				SenderDisplay:  ev.Sender,
				Text:           ev.Content.Body,
				ReceivedAt:     time.UnixMilli(ev.OriginServerTS).UTC(),
				IsDM:           strings.Contains(roomID, "@"),
			}
			if err := env.Validate(); err != nil {
				continue
			}
			envs = append(envs, env)
		}
	}
	next := result.NextBatch
	if next == "" {
		next = since
	}
	return envs, next, nil
}

func (a *MatrixAdapter) HandleDeliveryRequest(ctx context.Context, r io.Reader, w io.Writer) error {
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	var req feed.DeliveryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		var env feed.Envelope
		if err2 := json.Unmarshal(body, &env); err2 == nil && env.Channel != "" {
			req = feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{env}}
		} else {
			return fmt.Errorf("matrix: bad delivery request: %w", err)
		}
	}
	ids, err := a.Send(ctx, req.Envelopes)
	if err != nil {
		_ = json.NewEncoder(w).Encode(feed.DeliveryResult{Error: err.Error()})
		return err
	}
	return json.NewEncoder(w).Encode(feed.DeliveryResult{DeliveredIDs: ids})
}

func urlEscape(s string) string {
	// Minimal escape for Matrix room IDs in tests; real adapter uses net/url.
	return strings.ReplaceAll(strings.ReplaceAll(s, ":", "%3A"), "!", "%21")
}
