// Adapted from OpenClaw (https://github.com/openclaw/openclaw), MIT,
// tag v2026.10.2. Original TypeScript: extensions/discord/src/*.ts.
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

// DiscordAdapter implements outbound feed envelope delivery for Discord.
type DiscordAdapter struct {
	Token   string // Bot token — via bashy secret env (DISCORD_BOT_TOKEN), never argv
	BaseURL string // override for tests (default https://discord.com)
	Client  *http.Client
}

func (a *DiscordAdapter) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://discord.com"
}
func (a *DiscordAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Send delivers envelopes to Discord channels via POST /api/v10/channels/{id}/messages.
func (a *DiscordAdapter) Send(ctx context.Context, envs []feed.Envelope) ([]string, error) {
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

func (a *DiscordAdapter) sendOne(ctx context.Context, env feed.Envelope) (string, error) {
	u := fmt.Sprintf("%s/api/v10/channels/%s/messages", a.baseURL(), env.ConversationID)
	payload := map[string]string{
		"content": env.Text,
	}
	if env.ReplyTo != "" {
		payload["message_reference"] = env.ReplyTo
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("discord send: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.ID == "" {
		result.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return result.ID, nil
}

func (a *DiscordAdapter) HandleDeliveryRequest(ctx context.Context, r io.Reader, w io.Writer) error {
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
			return fmt.Errorf("discord: bad delivery request: %w", err)
		}
	}
	ids, err := a.Send(ctx, req.Envelopes)
	if err != nil {
		_ = json.NewEncoder(w).Encode(feed.DeliveryResult{Error: err.Error()})
		return err
	}
	return json.NewEncoder(w).Encode(feed.DeliveryResult{DeliveredIDs: ids})
}
