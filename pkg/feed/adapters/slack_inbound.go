// Slack Socket Mode protocol: https://docs.slack.dev/apis/events-api/using-socket-mode/
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/feed"
)

// Listen receives Slack Socket Mode events until cancellation. Pairing is
// required and checked before a file is downloaded or a handler is called.
func (a *SlackAdapter) Listen(ctx context.Context, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	if a.AppToken == "" || a.Token == "" || pairing == nil || handler == nil {
		return errors.New("slack: app token, bot token, pairing and handler are required")
	}
	for ctx.Err() == nil {
		err := a.listenSession(ctx, handler, pairing)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		backoff := 250 * time.Millisecond
		if err != nil {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return ctx.Err()
}

func (a *SlackAdapter) socketURL(ctx context.Context) (string, error) {
	if a.SocketURL != "" {
		return a.SocketURL, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL()+"/api/apps.connections.open", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.AppToken)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", errors.New("slack: socket discovery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("slack: socket discovery: %s", resp.Status)
	}
	var result struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", errors.New("slack: invalid socket discovery response")
	}
	if !result.OK || result.URL == "" {
		return "", errors.New("slack: socket discovery rejected")
	}
	return result.URL, nil
}

func (a *SlackAdapter) listenSession(ctx context.Context, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	u, err := a.socketURL(ctx)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return errors.New("slack: invalid socket URL")
	}
	if parsed.Scheme != "wss" && !(parsed.Scheme == "ws" && a.BaseURL != "" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return errors.New("slack: socket URL must use wss")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
	if err != nil {
		return errors.New("slack: socket dial failed")
	} // URL contains a ticket
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	for {
		var msg struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Type == "disconnect" {
			return nil
		}
		if msg.EnvelopeID == "" {
			continue
		}
		if msg.Type == "events_api" {
			if err := a.handleEvent(ctx, msg.Payload, handler, pairing); err != nil {
				return err
			}
		}
		// Slack requires an acknowledgement even for ignored and duplicate
		// envelopes. A failed handler is left unacknowledged for vendor retry.
		if err := conn.WriteJSON(map[string]string{"envelope_id": msg.EnvelopeID}); err != nil {
			return err
		}
	}
}

func (a *SlackAdapter) handleEvent(ctx context.Context, payload []byte, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	var p struct {
		EventID string `json:"event_id"`
		Event   struct {
			Type        string `json:"type"`
			Subtype     string `json:"subtype"`
			User        string `json:"user"`
			BotID       string `json:"bot_id"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			Text        string `json:"text"`
			Files       []struct {
				Name     string `json:"name"`
				MimeType string `json:"mimetype"`
				URL      string `json:"url_private_download"`
			} `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return errors.New("slack: malformed event")
	}
	m := p.Event
	if m.Type != "message" || (m.Subtype != "" && m.Subtype != "file_share") || m.BotID != "" || m.User == "" || m.Channel == "" || m.TS == "" || !pairing.IsPaired(m.User) {
		return nil
	}
	key := p.EventID
	if key == "" {
		key = m.Channel + ":" + m.TS
	}
	a.mu.Lock()
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	if a.seen[key] {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	seconds, _ := strconv.ParseFloat(m.TS, 64)
	received := time.Unix(0, int64(seconds*float64(time.Second))).UTC()
	if seconds == 0 {
		received = time.Now().UTC()
	}
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "slack", MessageID: m.Channel + ":" + m.TS, ConversationID: m.Channel, SenderID: m.User, Text: m.Text, ReplyTo: m.ThreadTS, ReceivedAt: received, IsDM: m.ChannelType == "im" || strings.HasPrefix(m.Channel, "D")}
	for _, f := range m.Files {
		if !strings.HasPrefix(f.MimeType, "image/") {
			continue
		}
		att, err := downloadPhoto(ctx, a.client(), f.URL, a.Token, f.Name, f.MimeType)
		if err != nil {
			for _, prior := range env.Attachments {
				_ = os.Remove(prior.Path)
			}
			return err
		}
		env.Attachments = append(env.Attachments, att)
	}
	if err := env.Validate(); err != nil {
		return err
	}
	if err := handler(ctx, env); err != nil {
		for _, att := range env.Attachments {
			_ = os.Remove(att.Path)
		}
		return err
	}
	a.mu.Lock()
	a.seen[key] = true
	a.mu.Unlock()
	return nil
}
