package feed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenClawDeclaration(t *testing.T) {
	// Verify the shipped openclaw.yaml is valid and maps envelope to CLI.
	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	if spec.Name != "openclaw" {
		t.Fatalf("name %q", spec.Name)
	}
	if spec.Outbound == nil || len(spec.Outbound.Argv) == 0 {
		t.Fatal("outbound argv required")
	}
	// Argv should contain openclaw message send
	got := strings.Join(spec.Outbound.Argv, " ")
	if !strings.Contains(got, "message") || !strings.Contains(got, "send") {
		t.Fatalf("argv %v should contain message send", spec.Outbound.Argv)
	}
	// Receipt should be via JSONPath or regex
	if spec.Outbound.Receipt == nil || (spec.Outbound.Receipt.JSONPath == "" && spec.Outbound.Receipt.Regex == "") {
		t.Fatal("receipt parsing required")
	}
	// Inbound mapping should exist
	if spec.Inbound == nil || len(spec.Inbound.Mapping) == 0 {
		t.Fatal("inbound mapping required")
	}
	// Validate round-trips
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestOpenClawWithFakeScript(t *testing.T) {
	// Fake openclaw: emulate `openclaw message send --target C --message TEXT`
	// reading stdin JSON and returning delivered_ids.
	tmpdir := t.TempDir()
	fakeOpenClaw := filepath.Join(tmpdir, "openclaw")
	script := `#!/bin/sh
# Fake openclaw: parse stdin if present, else use --message flag
INPUT=""
if [ ! -t 0 ]; then
  INPUT=$(cat)
fi
# Find --target and --message
TARGET=""; MSG=""
NEXT=""
for a in "$@"; do
  case "$NEXT" in
    target) TARGET="$a"; NEXT=""; continue;;
    message) MSG="$a"; NEXT=""; continue;;
  esac
  case "$a" in
    --target) NEXT="target";;
    --message) NEXT="message";;
  esac
done
# If stdin has envelopes, respond with their ids
if echo "$INPUT" | grep -q '"envelopes"'; then
  COUNT=$(echo "$INPUT" | grep -o '"message_id"' | wc -l | tr -d ' ')
  IDS=""
  for n in $(seq 1 $COUNT); do
    if [ -n "$IDS" ]; then IDS="$IDS,"; fi
    IDS="$IDS\"openclaw-msg-$n\""
  done
  echo "{\"delivered_ids\":[$IDS]}"
  exit 0
fi
# Single message via argv
if [ -n "$MSG" ]; then
  echo "{\"delivered_ids\":[\"openclaw-single\"]}"
else
  echo "{\"delivered_ids\":[\"openclaw-fallback\"]}"
fi
`
	if err := os.WriteFile(fakeOpenClaw, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	// Override tool bin to fake
	spec.Tool = &DeclTool{Bin: fakeOpenClaw}
	// Replace {{.Bin}} with fake bin path via outbound argv
	// Keep spec argv as declared but ensure first token becomes fake bin
	adapter, err := NewDeclAdapter(spec)
	if err != nil {
		t.Fatal(err)
	}

	env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m1", ConversationID: "chat-999", SenderID: "s1", Text: "hello openclaw", ReceivedAt: time.Now()}
	ids, err := adapter.Send(context.Background(), []Envelope{env})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one id")
	}
	// Should come from receipt JSONPath $.delivered_ids[0] -> openclaw-msg-1 or openclaw-single
	if ids[0] != "openclaw-msg-1" && ids[0] != "openclaw-single" && ids[0] != "openclaw-fallback" {
		t.Fatalf("unexpected id %q", ids[0])
	}

	// Batch via stdin json should yield per-envelope ids
	env2 := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m2", ConversationID: "chat-999", SenderID: "s1", Text: "second", ReceivedAt: time.Now()}
	ids, err = adapter.Send(context.Background(), []Envelope{env, env2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("batch ids %v", ids)
	}

	// Inbound JSON-lines via the declaration's mapping
	line := `{"channel":"openclaw","id":"oc-123","conversation":"chat-999","sender":"user-1","sender_display":"Alice","text":"inbound hello","received_at":"2026-10-14T00:00:00Z"}`
	inEnv, err := adapter.InboundEnvelopeFromJSON([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if inEnv.Text != "inbound hello" {
		t.Fatalf("text %q", inEnv.Text)
	}
}

func TestDeclWithOpenClawBinViaResolver(t *testing.T) {
	// When Tool.Bin is empty, binResolver should be used (binmgr fallback test).
	tmpdir := t.TempDir()
	fakeBin := filepath.Join(tmpdir, "fake-openclaw-resolve")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho '{\"delivered_ids\":[\"resolved\"]}'\n"), 0o755)
	spec := &DeclSpec{
		Name: "openclaw-resolve",
		Tool: &DeclTool{Name: "openclaw"},
		Outbound: &DeclOutbound{Argv: []string{fakeBin}},
	}
	adapter, _ := NewDeclAdapter(spec)
	called := false
	adapter.SetBinResolver(func(name string) string {
		if name == "openclaw" {
			called = true
			return fakeBin
		}
		return ""
	})
	env := Envelope{Version: ContractVersion, Channel: "openclaw-resolve", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
	ids, err := adapter.Send(context.Background(), []Envelope{env})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("binResolver not called")
	}
	if ids[0] != "resolved" {
		t.Fatalf("ids %v", ids)
	}
}
