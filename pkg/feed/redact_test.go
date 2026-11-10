package feed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// feedVaultFixture installs a vault-projected name with a rendered value in
// this process's environment; not credential-shaped, so only exact-value
// registration can mask it.
func feedVaultFixture(t *testing.T, localName, value string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(xdg, "bashy", "secrets.map")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(localName+"=@vault-item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(localName, value)
}

// Outbound channel text leaves the host entirely (Discord, Slack, Telegram,
// Matrix, declared tool commands). SendEnvelopes is the one choke every
// adapter delivers through, so the body the transport sees is masked there.
func TestSendEnvelopesRedactsVaultValues(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	feedVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	var delivered []Envelope
	SetEnvelopeSender(func(ctx context.Context, ch Channel, envelopes []Envelope) ([]string, error) {
		delivered = append(delivered, envelopes...)
		return nil, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	env := Envelope{
		Channel:        "telegram",
		MessageID:      "m-1",
		ConversationID: "conv-1",
		SenderID:       "sender-1",
		ReceivedAt:     time.Now().UTC(),
		Text:           "deploy note: the token is sycamore-lantern-42, handle with care",
	}
	if _, err := SendEnvelopes(context.Background(), Channel{Name: "ops", Cmd: "tool:send"}, []Envelope{env}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatalf("sender saw %d envelopes, want 1", len(delivered))
	}
	if strings.Contains(delivered[0].Text, "sycamore-lantern-42") {
		t.Fatalf("vault value left the host on the channel: %q", delivered[0].Text)
	}
	if !strings.Contains(delivered[0].Text, "[redacted:DEPLOY_TOKEN]") {
		t.Fatalf("outbound text missing marker: %q", delivered[0].Text)
	}
}

// Only exact values are masked; ordinary message text is delivered verbatim.
func TestSendEnvelopesLeavesOrdinaryText(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	feedVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	var delivered []Envelope
	SetEnvelopeSender(func(ctx context.Context, ch Channel, envelopes []Envelope) ([]string, error) {
		delivered = append(delivered, envelopes...)
		return nil, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	const body = "standup: blockers cleared, shipping the fix today"
	env := Envelope{
		Channel: "telegram", MessageID: "m-2", ConversationID: "conv-1",
		SenderID: "sender-1", ReceivedAt: time.Now().UTC(), Text: body,
	}
	if _, err := SendEnvelopes(context.Background(), Channel{Name: "ops", Cmd: "tool:send"}, []Envelope{env}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0].Text != body {
		t.Fatalf("ordinary text altered: %q", delivered[0].Text)
	}
}
