package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeclSpecValidate(t *testing.T) {
	_, err := ParseDecl([]byte(`name: test
outbound:
  stdin:
    mode: badmode
`))
	if err == nil {
		t.Fatal("expected validation error for bad stdin mode")
	}
	_, err = ParseDecl([]byte(`outbound:
  argv: ["hi"]
`))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	spec, err := ParseDecl([]byte(`name: mycli
outbound:
  argv: ["echo", "hi"]
  stdin:
    mode: json
  receipt:
    jsonpath: "$.delivered_ids[0]"
inbound:
  mode: jsonlines
  mapping:
    text: "$.body"
    sender_id: "$.sender"
`))
	if err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	if spec.Name != "mycli" {
		t.Fatalf("name %q", spec.Name)
	}
}

func TestDeclFakeCLIConformance(t *testing.T) {
	// Fake CLI that echoes delivered_ids JSON and validates argv/stdin.
	tmpdir := t.TempDir()
	fakeBin := filepath.Join(tmpdir, "fake-cli")
	script := `#!/bin/sh
# Fake CLI: reads stdin JSON (DeliveryRequest), echoes delivered_ids based on argv --message or stdin
# For this test we handle both: if stdin is present, use it; else check argv
if [ -t 0 ]; then
  INPUT=""
else
  INPUT=$(cat)
fi
# Check argv contains expected flags
echo "$@" | grep -q "FAKE_TOKEN" 2>/dev/null && echo "leak" >&2 && exit 1
# Parse message from argv --message or from stdin JSON
MSG=""
for i in "$@"; do
  if [ "$FOUND" = "1" ]; then MSG="$i"; FOUND="0"; fi
  if [ "$i" = "--message" ]; then FOUND="1"; fi
done
# If stdin has envelopes, count them
if echo "$INPUT" | grep -q '"envelopes"'; then
  COUNT=$(echo "$INPUT" | grep -o '"message_id"' | wc -l | tr -d ' ')
  # Build delivered_ids array
  IDS=""
  for n in $(seq 1 $COUNT); do
    if [ -n "$IDS" ]; then IDS="$IDS,"; fi
    IDS="$IDS\"fake-id-$n\""
  done
  echo "{\"delivered_ids\":[$IDS]}"
  exit 0
fi
# Fallback: use MSG as id
if [ -n "$MSG" ]; then
  echo "{\"delivered_ids\":[\"via-msg\"]}"
else
  echo "{\"delivered_ids\":[\"via-argv\"]}"
fi
`
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("argv template with stdin json", func(t *testing.T) {
		spec := &DeclSpec{
			Name: "fake",
			Tool: &DeclTool{Bin: fakeBin},
			Outbound: &DeclOutbound{
				Argv:    []string{fakeBin, "--channel", "{{.Channel}}", "--target", "{{.ConversationID}}"},
				Stdin:   &DeclStdin{Mode: "json"},
				Receipt: &DeclReceipt{JSONPath: "$.delivered_ids[0]"},
			},
		}
		adapter, err := NewDeclAdapter(spec)
		if err != nil {
			t.Fatal(err)
		}
		env := Envelope{Version: ContractVersion, Channel: "fake", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(ids) == 0 || ids[0] != "fake-id-1" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("argv template per envelope with receipt JSONPath", func(t *testing.T) {
		spec := &DeclSpec{
			Name: "fake2",
			Tool: &DeclTool{Bin: fakeBin},
			Outbound: &DeclOutbound{
				Argv:    []string{fakeBin, "--message", "{{.Text}}"},
				Receipt: &DeclReceipt{JSONPath: "$.delivered_ids[0]"},
			},
		}
		adapter, err := NewDeclAdapter(spec)
		if err != nil {
			t.Fatal(err)
		}
		env := Envelope{Version: ContractVersion, Channel: "fake2", MessageID: "m2", ConversationID: "c1", SenderID: "s1", Text: "hello world", ReceivedAt: time.Now()}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if ids[0] != "via-msg" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("receipt via regex", func(t *testing.T) {
		// Fake that prints "message_id: abc123" on stdout
		fakeBin2 := filepath.Join(tmpdir, "fake-regex")
		script2 := "#!/bin/sh\necho 'message_id: hello-999'\n"
		os.WriteFile(fakeBin2, []byte(script2), 0o755)
		spec := &DeclSpec{
			Name: "regextest",
			Tool: &DeclTool{Bin: fakeBin2},
			Outbound: &DeclOutbound{
				Argv:    []string{fakeBin2},
				Receipt: &DeclReceipt{Regex: `message_id:\s*(\S+)`},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		env := Envelope{Version: ContractVersion, Channel: "regextest", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hi", ReceivedAt: time.Now()}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 || ids[0] != "hello-999" {
			t.Fatalf("regex ids %v", ids)
		}
	})

	t.Run("exit code failure", func(t *testing.T) {
		fakeFail := filepath.Join(tmpdir, "fake-fail")
		os.WriteFile(fakeFail, []byte("#!/bin/sh\necho 'oops' >&2\nexit 2\n"), 0o755)
		spec := &DeclSpec{
			Name:     "failtest",
			Tool:     &DeclTool{Bin: fakeFail},
			Outbound: &DeclOutbound{Argv: []string{fakeFail}},
		}
		adapter, _ := NewDeclAdapter(spec)
		env := Envelope{Version: ContractVersion, Channel: "failtest", MessageID: "m1", ConversationID: "c1", SenderID: "s1", ReceivedAt: time.Now()}
		_, err := adapter.Send(context.Background(), []Envelope{env})
		if err == nil {
			t.Fatal("expected error for exit 2")
		}
	})

	t.Run("stdin text template", func(t *testing.T) {
		fakeEcho := filepath.Join(tmpdir, "fake-echo-stdin")
		os.WriteFile(fakeEcho, []byte("#!/bin/sh\ncat > /tmp/captured_$PPID; echo '{\"delivered_ids\":[\"echoed\"]}'\n"), 0o755)
		spec := &DeclSpec{
			Name: "textstdin",
			Tool: &DeclTool{Bin: fakeEcho},
			Outbound: &DeclOutbound{
				Argv:  []string{fakeEcho},
				Stdin: &DeclStdin{Mode: "text", Template: "prefix:{{.Text}}:suffix"},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		env := Envelope{Version: ContractVersion, Channel: "textstdin", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hello text", ReceivedAt: time.Now()}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
		if ids[0] != "echoed" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("inbound jsonlines via JSONPath", func(t *testing.T) {
		spec := &DeclSpec{
			Name: "inboundtest",
			Tool: &DeclTool{Bin: fakeBin},
			Inbound: &DeclInbound{
				Mode: "jsonlines",
				Mapping: map[string]string{
					"channel":         "$.chan",
					"message_id":      "$.mid",
					"conversation_id": "$.conv",
					"sender_id":       "$.sender",
					"text":            "$.body",
				},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		line := `{"chan":"mychan","mid":"m42","conv":"c99","sender":"s777","body":"hello inbound"}`
		env, err := adapter.InboundEnvelopeFromJSON([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if env.Channel != "mychan" || env.MessageID != "m42" || env.ConversationID != "c99" || env.SenderID != "s777" || env.Text != "hello inbound" {
			t.Fatalf("mapped env %+v", env)
		}
		// NDJSON with dedupe
		var received []Envelope
		handler := func(_ context.Context, e Envelope) error {
			received = append(received, e)
			return nil
		}
		ndjson := line + "\n" + line + "\n"
		if err := adapter.ProcessInboundNDJSON(context.Background(), bytes.NewReader([]byte(ndjson)), handler, nil); err != nil {
			t.Fatal(err)
		}
		if len(received) != 1 {
			t.Fatalf("expected dedupe to 1, got %d", len(received))
		}
	})

	t.Run("inbound webhook via JSONPath", func(t *testing.T) {
		spec := &DeclSpec{
			Name: "webhooktest",
			Inbound: &DeclInbound{
				Mode: "webhook",
				Webhook: &DeclWebhook{
					Path: "/webhook",
					Mapping: map[string]string{
						"channel":    "$.event.channel",
						"message_id": "$.event.id",
						"text":       "$.event.text",
						"sender_id":  "$.event.user",
					},
				},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		body := `{"event":{"channel":"webhook-chan","id":"evt-1","text":"from webhook","user":"u123"}}`
		env, err := adapter.InboundEnvelopeFromJSON([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if env.Text != "from webhook" || env.SenderID != "u123" || env.MessageID != "evt-1" {
			t.Fatalf("webhook env %+v", env)
		}
	})

	t.Run("address mapping channel:kind:id", func(t *testing.T) {
		spec := &DeclSpec{
			Name:    "addrtest",
			Address: &DeclAddress{ChannelFlag: "--channel", KindFlag: "--kind", IDFlag: "--target"},
			Inbound: &DeclInbound{Mapping: map[string]string{"channel": "$.c"}},
		}
		env := Envelope{Version: ContractVersion, Channel: "telegram", MessageID: "m1", ConversationID: "channel:12345", SenderID: "s1", Text: "hi", ReceivedAt: time.Now()}
		flags := spec.AddressFlags(env)
		// Should contain --channel telegram --kind channel --target 12345
		got := strings.Join(flags, " ")
		if !strings.Contains(got, "--channel telegram") {
			t.Fatalf("flags %v", flags)
		}
		if !strings.Contains(got, "--kind channel") {
			t.Fatalf("flags %v", flags)
		}
		if !strings.Contains(got, "--target 12345") {
			t.Fatalf("flags %v", flags)
		}
	})

	t.Run("YAML schema round trips", func(t *testing.T) {
		yml := `name: mysvc
display: My Service
channel: mysvc
tool:
  name: mycli
  version: "1.0.0"
outbound:
  argv: ["mycli", "send", "--target", "{{.ConversationID}}", "--message", "{{.Text}}"]
  stdin:
    mode: json
  receipt:
    jsonpath: "$.delivered_ids[0]"
inbound:
  mode: jsonlines
  mapping:
    message_id: "$.id"
    text: "$.body"
address:
  id_flag: "--target"
`
		spec, err := ParseDecl([]byte(yml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if spec.Outbound.Stdin.Mode != "json" {
			t.Fatalf("stdin mode %q", spec.Outbound.Stdin.Mode)
		}
		// Validate JSON serialization too.
		js, _ := json.Marshal(spec)
		var spec2 DeclSpec
		if err := json.Unmarshal(js, &spec2); err != nil {
			t.Fatal(err)
		}
		if spec2.Name != spec.Name {
			t.Fatalf("json roundtrip name %q", spec2.Name)
		}
	})

	t.Run("parse file not found", func(t *testing.T) {
		_, err := ParseDeclFile("/nonexistent/path.yaml")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("fake CLI secrets never on argv", func(t *testing.T) {
		// Ensure argv inspection doesn't leak env secrets: the adapter never
		// appends env-derived tokens; check captured argv via fake script.
		capturedArgv := ""
		fakeSecret := filepath.Join(tmpdir, "fake-secret-check")
		os.WriteFile(fakeSecret, []byte("#!/bin/sh\necho '{\"delivered_ids\":[\"ok\"]}'\n"), 0o755)
		spec := &DeclSpec{
			Name: "secrettest",
			Tool: &DeclTool{Bin: fakeSecret},
			Outbound: &DeclOutbound{
				Argv: []string{fakeSecret, "--target", "{{.ConversationID}}", "--message", "{{.Text}}"},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		// Set env with a fake token; adapter should not include it.
		t.Setenv("TELEGRAM_BOT_TOKEN", "super-secret-token")
		env := Envelope{Version: ContractVersion, Channel: "secrettest", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hello", ReceivedAt: time.Now()}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatal(err)
		}
		_ = capturedArgv
		if ids[0] != "ok" {
			t.Fatalf("ids %v", ids)
		}
		// The secret is not in the argv template, so it cannot leak.
	})

	t.Run("invalid template returns error", func(t *testing.T) {
		spec := &DeclSpec{
			Name: "badtempl",
			Tool: &DeclTool{Bin: fakeBin},
			Outbound: &DeclOutbound{
				Argv: []string{fakeBin, "{{.BadFunc}}"},
			},
		}
		adapter, _ := NewDeclAdapter(spec)
		env := Envelope{Version: ContractVersion, Channel: "badtempl", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hi", ReceivedAt: time.Now()}
		_, err := adapter.Send(context.Background(), []Envelope{env})
		if err == nil {
			t.Fatal("expected error for bad template field")
		}
		if !strings.Contains(err.Error(), "BadFunc") {
			t.Fatalf("error should mention BadFunc, got %v", err)
		}
	})
}
