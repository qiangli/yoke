package feed

import (
	"context"
	"testing"
	"time"
)

func TestBridgeTwoWayRoute(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1", TwoWay: true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	if err := SaveBridges(dir, routes); err != nil {
		t.Fatal(err)
	}

	var sent []Envelope
	SetEnvelopeSender(func(_ context.Context, ch Channel, envs []Envelope) ([]string, error) {
		sent = append(sent, envs...)
		ids := make([]string, len(envs))
		for i := range ids {
			ids[i] = "ok"
		}
		return ids, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	// Provide dst channels
	channels := []Channel{{Name: "telegram", Cmd: "dummy:send"}, {Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	// telegram -> matrix
	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
	if err := engine.Handle(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].ConversationID != "!room:1" {
		t.Fatalf("forward telegram->matrix: got %d sent %+v", len(sent), sent)
	}

	// matrix -> telegram (reverse via TwoWay)
	sent = nil
	env2 := Envelope{Version: ContractVersion, Channel: "matrix", MessageID: "m2", ConversationID: "!room:1", SenderID: "s1", Text: "reply", ReceivedAt: time.Now()}
	if err := engine.Handle(context.Background(), env2); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].ConversationID != "chat1" {
		t.Fatalf("reverse: got %d sent %+v", len(sent), sent)
	}
}

func TestBridgeLoopPrevention(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var sent int
	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		sent++
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "telegram", Cmd: "dummy:send"}, {Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	// Envelope already visited matrix:!room:1 in OriginChain => loop prevention drops it.
	env := Envelope{
		Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1",
		SenderID: "s1", Text: "loop", ReceivedAt: time.Now(),
		OriginChain: []string{"matrix:!room:1"},
	}
	if err := engine.Handle(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("loop not prevented: sent %d", sent)
	}
}

func TestBridgeDedupe(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var sent int
	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		sent++
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "telegram", Cmd: "dummy:send"}, {Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	_ = engine.Handle(context.Background(), env) // duplicate message_id
	if sent != 1 {
		t.Fatalf("dedupe failed: sent %d", sent)
	}
}

func TestBridgeAttribution(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var got Envelope
	SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
		got = envs[0]
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", SenderDisplay: "Alice", Text: "hello world", ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	if got.Text != "[Alice via telegram] hello world" {
		t.Fatalf("attribution text %q", got.Text)
	}
	if len(got.OriginChain) == 0 || got.OriginChain[0] != "telegram:chat1" {
		t.Fatalf("origin chain %+v", got.OriginChain)
	}
}

func TestBridgeAttachmentPassthrough(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var got Envelope
	SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
		got = envs[0]
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	att := Attachment{Path: "/tmp/foo.png", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MIME: "image/png", Size: 42}
	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "file", Attachments: []Attachment{att}, ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	if len(got.Attachments) != 1 || got.Attachments[0].SHA256 != att.SHA256 {
		t.Fatalf("attachments not passed: %+v", got.Attachments)
	}
}

func TestBridgeUnpairedRefused(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		t.Fatal("should not send for unpaired")
		return nil, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, DenyAllPairing{})

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
	err := engine.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected unpaired error")
	}
}

func TestBridgeDMNotBridged(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1", AllowDM: false,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var sent bool
	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		sent = true
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "dm", IsDM: true, ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	if sent {
		t.Fatal("DM should not be bridged without AllowDM")
	}
}

func TestBridgeDMAllowed(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1", AllowDM: true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var sent bool
	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		sent = true
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "dm", IsDM: true, ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	if !sent {
		t.Fatal("DM should be bridged with AllowDM")
	}
}

func TestBridgeRateLimit(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1", RateLimitPerMin: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var sent int
	SetEnvelopeSender(func(_ context.Context, _ Channel, _ []Envelope) ([]string, error) {
		sent++
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)
	now := time.Now()
	engine.now = func() time.Time { return now }

	env1 := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "chat1", SenderID: "s1", Text: "a", ReceivedAt: now}
	env2 := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m2", ConversationID: "chat1", SenderID: "s1", Text: "b", ReceivedAt: now}
	_ = engine.Handle(context.Background(), env1)
	_ = engine.Handle(context.Background(), env2)
	if sent != 1 {
		t.Fatalf("rate limit should drop second: sent %d", sent)
	}
}

func TestBridgeReplyThreading(t *testing.T) {
	dir := t.TempDir()
	routes := []BridgeRoute{{
		ID: "telegram:chat1->matrix:!room:1", SrcChannel: "telegram", SrcConversation: "chat1",
		DstChannel: "matrix", DstConversation: "!room:1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	_ = SaveBridges(dir, routes)

	var got Envelope
	SetEnvelopeSender(func(_ context.Context, _ Channel, envs []Envelope) ([]string, error) {
		got = envs[0]
		return []string{"ok"}, nil
	})
	t.Cleanup(func() { SetEnvelopeSender(nil) })

	channels := []Channel{{Name: "matrix", Cmd: "dummy:send"}}
	engine := NewBridgeEngine(dir, func() ([]Channel, error) { return channels, nil }, nil)

	env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m2", ConversationID: "chat1", SenderID: "s1", Text: "reply", ReplyTo: "m1", ReceivedAt: time.Now()}
	_ = engine.Handle(context.Background(), env)
	if got.ReplyTo != "m1" {
		t.Fatalf("reply_to not preserved: %q", got.ReplyTo)
	}
}
