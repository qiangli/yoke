package feed

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnvelopeContractVersion(t *testing.T) {
	if ContractVersion != "bashy-feed-envelope-v1" {
		t.Fatalf("version = %q", ContractVersion)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	env := Envelope{
		Version: ContractVersion, ID: "test-id", Channel: "telegram",
		MessageID: "123", ConversationID: "chat-1", SenderID: "user-1",
		SenderDisplay: "Alice", Text: "hello", ReplyTo: "122",
		ReceivedAt: time.Now().UTC().Truncate(time.Second),
	}
	data, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel != env.Channel || got.Text != env.Text {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, env)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	base := Envelope{
		Version: ContractVersion, Channel: "telegram", MessageID: "m1",
		ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now(),
	}
	tests := []struct {
		name string
		mut  func(Envelope) Envelope
		fail bool
	}{
		{"ok", func(e Envelope) Envelope { return e }, false},
		{"bad version", func(e Envelope) Envelope { e.Version = "bad"; return e }, true},
		{"no channel", func(e Envelope) Envelope { e.Channel = ""; return e }, true},
		{"no message_id", func(e Envelope) Envelope { e.MessageID = ""; return e }, true},
		{"no conversation", func(e Envelope) Envelope { e.ConversationID = ""; return e }, true},
		{"no sender", func(e Envelope) Envelope { e.SenderID = ""; return e }, true},
		{"zero time", func(e Envelope) Envelope { e.ReceivedAt = time.Time{}; return e }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.mut(base)
			err := env.Validate()
			if tc.fail && err == nil {
				t.Fatal("expected error")
			}
			if !tc.fail && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAttachmentValidate(t *testing.T) {
	good := Attachment{Path: "/tmp/foo.png", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MIME: "image/png"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good attachment: %v", err)
	}
	bad := good
	bad.SHA256 = "nothex"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected hex error")
	}
}

func TestDeliveryRequestJSON(t *testing.T) {
	req := DeliveryRequest{Version: ContractVersion, Envelopes: []Envelope{
		{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()},
	}}
	data, _ := json.Marshal(req)
	var got DeliveryRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Envelopes) != 1 {
		t.Fatalf("envelopes len %d", len(got.Envelopes))
	}
}

func TestChainContains(t *testing.T) {
	chain := []string{"telegram:chat1", "matrix:!room"}
	if !ChainContains(chain, "telegram", "chat1") {
		t.Fatal("should contain")
	}
	if ChainContains(chain, "telegram", "chat2") {
		t.Fatal("should not contain")
	}
}

func TestOriginKey(t *testing.T) {
	env := Envelope{Channel: "telegram", MessageID: "m1"}
	if env.OriginKey() != "telegram:m1" {
		t.Fatalf("key %q", env.OriginKey())
	}
}
