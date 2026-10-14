// Adapted from OpenClaw (https://github.com/openclaw/openclaw), MIT,
// tag v2026.10.2. Original TypeScript: extensions/slack/src/*.ts.
// Outbound only in this stage; inbound is next stage. Converted to Go.

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

// SlackAdapter implements outbound feed envelope delivery for Slack.
type SlackAdapter struct {
	Token   string // Bot token — via bashy secret env (SLACK_BOT_TOKEN), never argv
	BaseURL string // override for tests (default https://slack.com)
	Client  *http.Client
}

func (a *SlackAdapter) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://slack.com"
}
func (a *SlackAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Send delivers envelopes to Slack via chat.postMessage.
func (a *SlackAdapter) Send(ctx context.Context, envs []feed.Envelope) ([]string, error) {
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

func (a *SlackAdapter) sendOne(ctx context.Context, env feed.Envelope) (string, error) {
	u := fmt.Sprintf("%s/api/chat.postMessage", a.baseURL())
	payload := map[string]string{
		"channel": env.ConversationID,
		"text":    env.Text,
	}
	if env.ReplyTo != "" {
		payload["thread_ts"] = env.ReplyTo
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
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
		return "", fmt.Errorf("slack postMessage: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		OK  bool   `json:"ok"`
		TS  string `json:"ts"`
		Err string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("slack postMessage: %s: %s", result.Err, string(raw))
	}
	if result.TS == "" {
		result.TS = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return result.TS, nil
}

func (a *SlackAdapter) HandleDeliveryRequest(ctx context.Context, r io.Reader, w io.Writer) error {
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
			return fmt.Errorf("slack: bad delivery request: %w", err)
		}
	}
	ids, err := a.Send(ctx, req.Envelopes)
	if err != nil {
		_ = json.NewEncoder(w).Encode(feed.DeliveryResult{Error: err.Error()})
		return err
	}
	return json.NewEncoder(w).Encode(feed.DeliveryResult{DeliveredIDs: ids})
}
