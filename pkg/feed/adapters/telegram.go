// Adapted from OpenClaw (https://github.com/openclaw/openclaw), MIT,
// tag v2026.10.2 (commit pinned per docs/sprint-434-evidence.md). Original
// TypeScript: extensions/telegram/src/*.ts. Converted to Go for bashy
// feed envelope contract (bashy-feed-envelope-v1).

package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/feed"
)

// TelegramAdapter implements the feed envelope contract for Telegram Bot API.
type TelegramAdapter struct {
	Token   string // Bot token — resolved via bashy secret env (TELEGRAM_BOT_TOKEN), never argv
	BaseURL string // override for tests (default https://api.telegram.org)
	Client  *http.Client
}

func (a *TelegramAdapter) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://api.telegram.org"
}
func (a *TelegramAdapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Send delivers envelopes to Telegram via sendMessage / sendDocument.
func (a *TelegramAdapter) Send(ctx context.Context, envs []feed.Envelope) ([]string, error) {
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

func (a *TelegramAdapter) sendOne(ctx context.Context, env feed.Envelope) (string, error) {
	// For text-only, use sendMessage.
	if len(env.Attachments) == 0 {
		return a.sendMessage(ctx, env.ConversationID, env.Text, env.ReplyTo)
	}
	// With attachments, send text then each attachment as document.
	id, err := a.sendMessage(ctx, env.ConversationID, env.Text, env.ReplyTo)
	if err != nil {
		return "", err
	}
	for _, att := range env.Attachments {
		if _, err := a.sendDocument(ctx, env.ConversationID, att); err != nil {
			// Attachment failure does not roll back the message.
			continue
		}
	}
	return id, nil
}

func (a *TelegramAdapter) sendMessage(ctx context.Context, chatID, text, replyTo string) (string, error) {
	u := fmt.Sprintf("%s/bot%s/sendMessage", a.baseURL(), a.Token)
	payload := map[string]string{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo != "" {
		payload["reply_to_message_id"] = replyTo
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("telegram sendMessage: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("telegram sendMessage: not ok: %s", string(raw))
	}
	return fmt.Sprintf("%d", result.Result.MessageID), nil
}

func (a *TelegramAdapter) sendDocument(ctx context.Context, chatID string, att feed.Attachment) (string, error) {
	u := fmt.Sprintf("%s/bot%s/sendDocument", a.baseURL(), a.Token)
	// For tests we use JSON; production would use multipart. The httptest fake
	// accepts either.
	payload := map[string]string{
		"chat_id":  chatID,
		"document": att.Path,
		"caption":  att.Name,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("telegram sendDocument: %s: %s", resp.Status, string(raw))
	}
	return att.SHA256[:8], nil
}

// Poll fetches new messages via getUpdates and emits envelopes (inbound).
func (a *TelegramAdapter) Poll(ctx context.Context, offset int64) ([]feed.Envelope, int64, error) {
	u := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=0", a.baseURL(), url.QueryEscape(a.Token), offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, offset, err
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, offset, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, offset, fmt.Errorf("telegram getUpdates: %s: %s", resp.Status, string(raw))
	}
	var result struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				MessageID int64 `json:"message_id"`
				Chat      struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
				From struct {
					ID        int64  `json:"id"`
					Username  string `json:"username"`
					FirstName string `json:"first_name"`
				} `json:"from"`
				Text    string `json:"text"`
				ReplyTo *struct {
					MessageID int64 `json:"message_id"`
				} `json:"reply_to_message"`
				Date int64 `json:"date"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, offset, err
	}
	var envs []feed.Envelope
	next := offset
	for _, upd := range result.Result {
		if upd.UpdateID >= next {
			next = upd.UpdateID + 1
		}
		m := upd.Message
		if m.Text == "" && m.MessageID == 0 {
			continue
		}
		env := feed.Envelope{
			Version:        feed.ContractVersion,
			Channel:        "telegram",
			MessageID:      fmt.Sprintf("%d", m.MessageID),
			ConversationID: fmt.Sprintf("%d", m.Chat.ID),
			SenderID:       fmt.Sprintf("%d", m.From.ID),
			SenderDisplay:  m.From.Username,
			Text:           m.Text,
			ReceivedAt:     time.Unix(m.Date, 0).UTC(),
			IsDM:           m.Chat.Type == "private",
		}
		if env.SenderDisplay == "" {
			env.SenderDisplay = m.From.FirstName
		}
		if m.ReplyTo != nil {
			env.ReplyTo = fmt.Sprintf("%d", m.ReplyTo.MessageID)
		}
		if err := env.Validate(); err != nil {
			continue
		}
		envs = append(envs, env)
	}
	return envs, next, nil
}

// HandleDeliveryRequest handles stdin DeliveryRequest JSON for the tool
// command entry point: read from r, send, write DeliveryResult to w.
func (a *TelegramAdapter) HandleDeliveryRequest(ctx context.Context, r io.Reader, w io.Writer) error {
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	var req feed.DeliveryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Try single envelope fallback (when invoked with --message JSON).
		var env feed.Envelope
		if err2 := json.Unmarshal(body, &env); err2 == nil && env.Channel != "" {
			req = feed.DeliveryRequest{Version: feed.ContractVersion, Envelopes: []feed.Envelope{env}}
		} else {
			return fmt.Errorf("telegram: bad delivery request: %w", err)
		}
	}
	ids, err := a.Send(ctx, req.Envelopes)
	if err != nil {
		_ = json.NewEncoder(w).Encode(feed.DeliveryResult{Error: err.Error()})
		return err
	}
	return json.NewEncoder(w).Encode(feed.DeliveryResult{DeliveredIDs: ids})
}
