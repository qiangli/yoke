package feed

// Envelope is the symmetric channel adapter contract.
//
// One shape travels both directions: core → adapter (outbound) and
// adapter → core (inbound). Versioned so adapters can negotiate without
// a bashy release.
//
//   - Outbound: the channel's registered tool command receives a JSON array
//     of envelopes on stdin. It returns JSON {delivered_ids: []string} on
//     stdout. Secrets are injected via the existing secret path
//     (bashy secret env → env vars), never argv.
//   - Inbound: adapters emit newline-delimited JSON envelopes on stdout of a
//     supervised bashy service unit, or POST them to a loopback endpoint
//     (127.0.0.1). The core validates, applies pairing/quiet/cursor/draft
//     policy, then fans out through bridge routes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ContractVersion is the envelope contract version. Adapters MUST send it;
// core rejects envelopes with an unknown version.
const ContractVersion = "bashy-feed-envelope-v1"

// Envelope is one message in either direction.
type Envelope struct {
	// Version is the contract version. Must equal ContractVersion.
	Version string `json:"version"`

	// ID is the core-assigned stable id for this envelope (uuid v7).
	// Origin adapters may leave it empty; core fills it on inbound.
	ID string `json:"id"`

	// Channel is the origin channel name (e.g. "telegram", "matrix").
	Channel string `json:"channel"`

	// MessageID is the origin channel's native message identifier.
	MessageID string `json:"message_id"`

	// ConversationID is the room/channel/conversation identifier on the
	// origin channel (e.g. Telegram chat_id, Matrix room_id).
	ConversationID string `json:"conversation_id"`

	// SenderID is the origin sender's stable identifier.
	SenderID string `json:"sender_id"`
	// SenderDisplay is the human-readable display name.
	SenderDisplay string `json:"sender_display,omitempty"`

	// Text is the message body (plain text / markdown).
	Text string `json:"text"`

	// Attachments are files local to the adapter host at delivery time.
	Attachments []Attachment `json:"attachments,omitempty"`

	// ReplyTo is the MessageID being replied to, if any.
	ReplyTo string `json:"reply_to,omitempty"`

	// ReceivedAt is when the origin channel received the message.
	ReceivedAt time.Time `json:"received_at"`

	// OriginChain tracks the path this envelope has already traversed
	// (for bridge loop prevention). Each hop appends "channel:conversation".
	OriginChain []string `json:"origin_chain,omitempty"`

	// IsDM marks whether the conversation is a direct message. DMs are
	// never bridged unless the route explicitly allows it.
	IsDM bool `json:"is_dm,omitempty"`
}

// ChannelAttachment is the envelope's attachment file (aliased to Attachment
// for the signed-post path, but with JSON/mime semantics for the channel
// contract). The common Attachment type in attach.go carries both.
type ChannelAttachment = Attachment

// DeliveryRequest is what the core sends to an adapter on stdin (outbound).
type DeliveryRequest struct {
	Version   string     `json:"version"`
	Envelopes []Envelope `json:"envelopes"`
}

// DeliveryResult is what an adapter returns on stdout (outbound).
type DeliveryResult struct {
	DeliveredIDs []string `json:"delivered_ids"`
	// Error is set when the adapter could not deliver any envelope.
	Error string `json:"error,omitempty"`
}

// Validate checks envelope fields for contract conformance.
func (e *Envelope) Validate() error {
	if e.Version != ContractVersion {
		return fmt.Errorf("feed: envelope version %q != %q", e.Version, ContractVersion)
	}
	if strings.TrimSpace(e.Channel) == "" {
		return errors.New("feed: envelope channel is required")
	}
	if strings.TrimSpace(e.MessageID) == "" {
		return errors.New("feed: envelope message_id is required")
	}
	if strings.TrimSpace(e.ConversationID) == "" {
		return errors.New("feed: envelope conversation_id is required")
	}
	if strings.TrimSpace(e.SenderID) == "" {
		return errors.New("feed: envelope sender_id is required")
	}
	if e.ReceivedAt.IsZero() {
		return errors.New("feed: envelope received_at is required")
	}
	for _, a := range e.Attachments {
		if err := a.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks attachment fields. MIME is optional for feed post attachments
// (legacy) but required for channel envelope attachments; this check is lenient.
func (a *Attachment) Validate() error {
	if strings.TrimSpace(a.Path) == "" {
		return errors.New("feed: attachment path is required")
	}
	if strings.TrimSpace(a.SHA256) == "" {
		return errors.New("feed: attachment sha256 is required")
	}
	if len(a.SHA256) != 64 {
		return fmt.Errorf("feed: attachment sha256 must be 64 hex chars, got %d", len(a.SHA256))
	}
	if _, err := hex.DecodeString(a.SHA256); err != nil {
		return fmt.Errorf("feed: attachment sha256 is not hex: %w", err)
	}
	return nil
}

// NewAttachment builds an Attachment from a local file, computing sha256.
func NewAttachment(path, mime string) (Attachment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, fmt.Errorf("feed: reading attachment %q: %w", path, err)
	}
	h := sha256.Sum256(data)
	return Attachment{
		Path:   path,
		SHA256: hex.EncodeToString(h[:]),
		MIME:   mime,
		Name:   filepath.Base(path),
		Size:   int64(len(data)),
	}, nil
}

// MarshalEnvelope encodes one envelope as JSON (single line for NDJSON).
func MarshalEnvelope(e Envelope) ([]byte, error) {
	if e.Version == "" {
		e.Version = ContractVersion
	}
	return json.Marshal(e)
}

// UnmarshalEnvelope decodes and validates one envelope.
func UnmarshalEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("feed: envelope json: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// OriginKey returns the dedupe key for this envelope: channel + message_id.
func (e *Envelope) OriginKey() string {
	return e.Channel + ":" + e.MessageID
}

// ChainContains reports whether originChain already contains channel:conversation.
func ChainContains(chain []string, channel, conversation string) bool {
	key := channel + ":" + conversation
	for _, c := range chain {
		if c == key {
			return true
		}
	}
	return false
}
