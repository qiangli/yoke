// Discord Gateway protocol: https://discord.com/developers/docs/events/gateway
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/feed"
)

// Listen receives Discord Gateway MESSAGE_CREATE events until ctx is cancelled.
// handler is normally the existing feed bridge handler. A non-nil pairing
// checker is required: unpaired senders never reach the handler or photo URL.
func (a *DiscordAdapter) Listen(ctx context.Context, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	if a.Token == "" || pairing == nil || handler == nil {
		return errors.New("discord: token, pairing and handler are required")
	}
	for ctx.Err() == nil {
		err := a.listenSession(ctx, handler, pairing)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Gateway disconnects and reconnect requests are normal. A short delay
		// prevents a failed endpoint from becoming a busy retry loop.
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

type discordPacket struct {
	Op int             `json:"op"`
	T  string          `json:"t"`
	S  *int64          `json:"s"`
	D  json.RawMessage `json:"d"`
}

func (a *DiscordAdapter) gatewayURL(ctx context.Context) (string, error) {
	a.mu.Lock()
	u := a.resumeURL
	a.mu.Unlock()
	if u != "" {
		return u, nil
	}
	if a.GatewayURL != "" {
		return a.GatewayURL, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+"/api/v10/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord gateway discovery: %s", resp.Status)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.URL == "" {
		return "", errors.New("discord gateway discovery: missing URL")
	}
	return result.URL, nil
}

func (a *DiscordAdapter) listenSession(ctx context.Context, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	u, err := a.gatewayURL(ctx)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	if parsed.Scheme != "wss" && !(parsed.Scheme == "ws" && (a.GatewayURL != "" || a.BaseURL != "") && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return errors.New("discord: gateway URL must use wss")
	}
	q := parsed.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	parsed.RawQuery = q.Encode()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, parsed.String(), nil)
	if err != nil {
		return errors.New("discord: gateway dial failed")
	} // URL may contain credentials
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
	var writeMu sync.Mutex
	var heartbeatCancel context.CancelFunc
	defer func() {
		if heartbeatCancel != nil {
			heartbeatCancel()
		}
	}()
	var ackMu sync.Mutex
	lastAck := true
	for {
		var p discordPacket
		err := conn.ReadJSON(&p)
		if err != nil {
			return err
		}
		switch p.Op {
		case 10: // HELLO
			var hello struct {
				HeartbeatInterval int64 `json:"heartbeat_interval"`
			}
			if err := json.Unmarshal(p.D, &hello); err != nil || hello.HeartbeatInterval <= 0 {
				return errors.New("discord: invalid hello")
			}
			if heartbeatCancel != nil {
				heartbeatCancel()
			}
			hbCtx, cancel := context.WithCancel(ctx)
			heartbeatCancel = cancel
			go func() {
				ticker := time.NewTicker(time.Duration(hello.HeartbeatInterval) * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-hbCtx.Done():
						return
					case <-ticker.C:
						ackMu.Lock()
						acknowledged := lastAck
						lastAck = false
						ackMu.Unlock()
						if !acknowledged {
							_ = conn.Close()
							return
						}
						a.mu.Lock()
						seq := a.sequence
						a.mu.Unlock()
						writeMu.Lock()
						err := conn.WriteJSON(map[string]any{"op": 1, "d": seq})
						writeMu.Unlock()
						if err != nil {
							_ = conn.Close()
							return
						}
					}
				}
			}()
			a.mu.Lock()
			session, seq := a.sessionID, a.sequence
			a.mu.Unlock()
			writeMu.Lock()
			if session != "" {
				err = conn.WriteJSON(map[string]any{"op": 6, "d": map[string]any{"token": a.Token, "session_id": session, "seq": seq}})
			} else {
				// GUILD_MESSAGES, DIRECT_MESSAGES and privileged MESSAGE_CONTENT.
				err = conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": a.Token, "intents": (1 << 9) | (1 << 12) | (1 << 15), "properties": map[string]string{"os": "bashy", "browser": "bashy", "device": "bashy"}}})
			}
			writeMu.Unlock()
			if err != nil {
				return err
			}
		case 11:
			ackMu.Lock()
			lastAck = true
			ackMu.Unlock()
		case 1: // server requests an immediate heartbeat
			a.mu.Lock()
			seq := a.sequence
			a.mu.Unlock()
			writeMu.Lock()
			err := conn.WriteJSON(map[string]any{"op": 1, "d": seq})
			writeMu.Unlock()
			if err != nil {
				return err
			}
		case 7:
			return nil // RECONNECT, keep resume state
		case 9: // INVALID_SESSION
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				a.mu.Lock()
				a.sessionID = ""
				a.resumeURL = ""
				a.sequence = 0
				a.mu.Unlock()
			}
			return nil
		case 0:
			if p.T == "READY" {
				var ready struct {
					SessionID string `json:"session_id"`
					ResumeURL string `json:"resume_gateway_url"`
				}
				if json.Unmarshal(p.D, &ready) == nil {
					a.mu.Lock()
					a.sessionID = ready.SessionID
					a.resumeURL = ready.ResumeURL
					a.mu.Unlock()
				}
			} else if p.T == "MESSAGE_CREATE" {
				if err := a.handleMessage(ctx, p.D, handler, pairing); err != nil {
					return err
				}
			}
		}
		// Only advance the resume cursor after the handler succeeds. A
		// failed delivery can then be replayed by Gateway resume.
		if p.S != nil {
			a.mu.Lock()
			a.sequence = *p.S
			a.mu.Unlock()
		}
	}
}

func (a *DiscordAdapter) handleMessage(ctx context.Context, data []byte, handler feed.InboundHandler, pairing feed.PairingChecker) error {
	var m struct {
		ID        string    `json:"id"`
		ChannelID string    `json:"channel_id"`
		GuildID   string    `json:"guild_id"`
		Content   string    `json:"content"`
		Timestamp time.Time `json:"timestamp"`
		Author    struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"author"`
		MessageReference struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
		Attachments []struct {
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			URL         string `json:"url"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return errors.New("discord: malformed message")
	}
	if m.Author.Bot || m.ID == "" || m.ChannelID == "" || m.Author.ID == "" || !pairing.IsPaired(m.Author.ID) {
		return nil
	}
	a.mu.Lock()
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	if a.seen[m.ID] {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	env := feed.Envelope{Version: feed.ContractVersion, Channel: "discord", MessageID: m.ID, ConversationID: m.ChannelID, SenderID: m.Author.ID, SenderDisplay: m.Author.Username, Text: m.Content, ReplyTo: m.MessageReference.MessageID, ReceivedAt: m.Timestamp, IsDM: m.GuildID == ""}
	if env.ReceivedAt.IsZero() {
		env.ReceivedAt = time.Now().UTC()
	}
	for _, att := range m.Attachments {
		if !strings.HasPrefix(att.ContentType, "image/") {
			continue
		}
		photo, err := downloadPhoto(ctx, a.client(), att.URL, "", att.Filename, att.ContentType)
		if err != nil {
			for _, prior := range env.Attachments {
				_ = os.Remove(prior.Path)
			}
			return err
		}
		env.Attachments = append(env.Attachments, photo)
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
	a.seen[m.ID] = true
	a.mu.Unlock()
	return nil
}
