package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOpenClawDeclaration(t *testing.T) {
	// The shipped openclaw.yaml is embedded (resolves independent of cwd);
	// ParseDeclFile must agree with the embedded registration.
	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	if got := DeclForChannel("openclaw"); got == nil || got.Name != "openclaw" {
		t.Fatalf("embedded openclaw decl not registered")
	}
	// The embedded declaration resolves independent of cwd: from an
	// unrelated directory the registry must still serve the spec without
	// touching the filesystem.
	t.Chdir(t.TempDir())
	if got := DeclForChannel("openclaw"); got == nil || got.Name != "openclaw" {
		t.Fatalf("embedded openclaw decl lost after chdir")
	}
	if spec.Name != "openclaw" {
		t.Fatalf("name %q", spec.Name)
	}
	if spec.Outbound == nil || len(spec.Outbound.Argv) == 0 {
		t.Fatal("outbound argv required")
	}
	// Argv must mirror the real `openclaw message send` contract:
	// --channel/--target/--message plus --json for the receipt.
	got := strings.Join(spec.Outbound.Argv, " ")
	for _, want := range []string{"message", "send", "--channel", "--target", "--message", "--json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %v should contain %q", spec.Outbound.Argv, want)
		}
	}
	// The real CLI takes argv only: no stdin batching (one target per
	// invocation; batching would send every envelope to the first target).
	if spec.Outbound.Stdin != nil {
		t.Fatalf("stdin must be unset for the real CLI, got %+v", spec.Outbound.Stdin)
	}
	// Receipt is the real --json shape: top-level messageId.
	if spec.Outbound.Receipt == nil || spec.Outbound.Receipt.JSONPath != "$.messageId" {
		t.Fatalf("receipt must be $.messageId, got %+v", spec.Outbound.Receipt)
	}
	// Tool pins the exact reviewed npm release with integrity.
	if spec.Tool == nil || spec.Tool.Name != "openclaw" || spec.Tool.Version != "2026.10.2" {
		t.Fatalf("tool must pin openclaw 2026.10.2, got %+v", spec.Tool)
	}
	for platform, asset := range spec.Tool.Assets {
		if asset.URL != "https://registry.npmjs.org/openclaw/-/openclaw-2026.10.2.tgz" {
			t.Fatalf("platform %s: wrong tarball %q", platform, asset.URL)
		}
		if strings.TrimSpace(asset.SHA512) == "" {
			t.Fatalf("platform %s: missing integrity", platform)
		}
		if !asset.Tree || asset.Entrypoint == "" {
			t.Fatalf("platform %s: npm tree asset needs an entrypoint", platform)
		}
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// openClawCLIFake uses the test executable as a portable CLI fixture. The
// declaration still renders the real OpenClaw arguments after the helper flags.
func openClawCLIFake(t *testing.T, spec *DeclSpec, mode string) string {
	t.Helper()
	t.Setenv("BASHY_TEST_OPENCLAW_MODE", mode)
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec.Outbound.Argv = append([]string{spec.Outbound.Argv[0], "-test.run=^TestOpenClawCLIHelper$", "--"}, spec.Outbound.Argv[1:]...)
	return path
}

func TestOpenClawCLIHelper(t *testing.T) {
	mode := os.Getenv("BASHY_TEST_OPENCLAW_MODE")
	if mode == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return // Parent test process, not a CLI invocation.
	}
	args = args[1:]
	switch mode {
	case "garbage":
		fmt.Println("not json at all")
	case "via-node":
		fmt.Println(`{"messageId":"via-node"}`)
	case "resolved":
		fmt.Println(`{"messageId":"resolved"}`)
	case "contract":
		hasSend, hasJSON := false, false
		values := map[string]string{}
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "send":
				hasSend = true
			case "--json":
				hasJSON = true
			case "--target", "--message", "--channel":
				if i+1 < len(args) {
					values[args[i]] = args[i+1]
					i++
				}
			}
		}
		if !hasSend || !hasJSON || values["--target"] == "" || values["--message"] == "" {
			fmt.Fprintln(os.Stderr, "bad argv: missing send/json/target/message")
			os.Exit(2)
		}
		id := "mid-for-" + values["--target"]
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"action": "send", "channel": values["--channel"], "messageId": id,
			"payload": map[string]string{"messageId": id},
		}); err != nil {
			os.Exit(2)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestOpenClawContractArgs(t *testing.T) {
	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	fakeBin := openClawCLIFake(t, spec, "contract")
	spec.Tool = &DeclTool{Bin: fakeBin} // fake injection, not the pinned bundle
	adapter, err := NewDeclAdapter(spec)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("prefixed target passes channel/target/message", func(t *testing.T) {
		env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m1",
			ConversationID: "telegram:channel:123", SenderID: "s1",
			Text: "hello openclaw", ReceivedAt: time.Now()}
		argv, err := adapter.renderArgv(env, fakeBin, false)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv, " ")
		for _, want := range []string{"message send", "--channel telegram", "--target channel:123", "--message hello openclaw", "--json"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("argv %q should contain %q", joined, want)
			}
		}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(ids) != 1 || ids[0] != "mid-for-channel:123" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("unprefixed target omits channel", func(t *testing.T) {
		env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m2",
			ConversationID: "chat-999", SenderID: "s1",
			Text: "plain target", ReceivedAt: time.Now()}
		argv, err := adapter.renderArgv(env, fakeBin, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range argv {
			if tok == "--channel" {
				t.Fatalf("unprefixed target must omit --channel, argv %v", argv)
			}
		}
		ids, err := adapter.Send(context.Background(), []Envelope{env})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(ids) != 1 || ids[0] != "mid-for-chat-999" {
			t.Fatalf("ids %v", ids)
		}
	})

	t.Run("colon ids without provider prefix pass through whole", func(t *testing.T) {
		// Matrix-style ids contain colons but no provider prefix: they must
		// not be split, and no --channel may be invented for them.
		for _, conv := range []string{"!room:example.org", "@user:example.org"} {
			env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m3",
				ConversationID: conv, SenderID: "s1", Text: "matrix",
				ReceivedAt: time.Now()}
			argv, err := adapter.renderArgv(env, fakeBin, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(argv, " "), "--target "+conv) {
				t.Fatalf("argv %v should target %q whole", argv, conv)
			}
			for _, tok := range argv {
				if tok == "--channel" {
					t.Fatalf("non-provider id must omit --channel, argv %v", argv)
				}
			}
		}
	})
}

func TestOpenClawWrongBytesRejected(t *testing.T) {
	// Garbage stdout is not JSON: receipt parsing must fail, not invent ids.
	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	garbage := openClawCLIFake(t, spec, "garbage")
	spec.Tool = &DeclTool{Bin: garbage}
	adapter, err := NewDeclAdapter(spec)
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m1",
		ConversationID: "telegram:123", SenderID: "s1", Text: "hi",
		ReceivedAt: time.Now()}
	if _, err := adapter.Send(context.Background(), []Envelope{env}); err == nil {
		t.Fatal("expected error for non-JSON receipt bytes")
	}
}

func TestDeclWithOpenClawBinViaResolver(t *testing.T) {
	// When Tool.Bin is empty, binResolver should be used (binmgr fallback test).
	spec := &DeclSpec{
		Name: "openclaw-resolve",
		Tool: &DeclTool{Name: "openclaw"},
		Outbound: &DeclOutbound{
			Argv:    []string{"{{.Bin}}", "message", "send", "--target", "{{.Target}}", "--message", "{{.Text}}", "--json"},
			Receipt: &DeclReceipt{JSONPath: "$.messageId"},
		},
	}
	fakeBin := openClawCLIFake(t, spec, "resolved")
	adapter, _ := NewDeclAdapter(spec)
	called := false
	adapter.SetBinResolver(func(name string) string {
		if name == "openclaw" {
			called = true
			return fakeBin
		}
		return ""
	})
	env := Envelope{Version: ContractVersion, Channel: "openclaw-resolve", MessageID: "m1", ConversationID: "c1", SenderID: "s1", Text: "hi", ReceivedAt: time.Now()}
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
