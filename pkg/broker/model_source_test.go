package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDoorModelImportAndRotation(t *testing.T) {
	t.Setenv("BASHY_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	useFakeOutpost(t)
	g := DoorGrant{Schema: GrantSchemaVersion, Door: "dragon", Peer: "elephant", CloudboxURL: "https://example.test/app/llm", Credential: "bpeer_first"}
	for _, key := range []string{"bpeer_first", "bpeer_rotated"} {
		g.Credential = key
		if key == "bpeer_rotated" {
			t.Setenv("OPENAI_API_KEY", "stale-env")
		}
		data, _ := json.Marshal(g)
		out, err := runShareLLM(t, string(data), "door", "import")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "@dragon") {
			t.Fatalf("import gives no model source instruction: %s", out)
		}
		for _, name := range []string{"codex-gpt6-sol@dragon", "dragon-door-codex-gpt6-sol"} {
			m, ok, err := ResolveDoorModel(context.Background(), name)
			if err != nil || !ok {
				t.Fatalf("resolve %s: %v %v", name, ok, err)
			}
			if m.ModelID != "codex-gpt6-sol" || m.Key != key || m.BaseURL != g.CloudboxURL+"/k/"+key+"/v1" {
				t.Fatalf("incorrect model or stale credential for %s", name)
			}
		}
	}
}

func TestDoorModelWSLStore(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	oldWSL, oldInterop, oldGateway := inWSL, wslInterop, wslGateway
	t.Cleanup(func() { inWSL, wslInterop, wslGateway = oldWSL, oldInterop, oldGateway })
	inWSL = func() bool { return true }
	wslGateway = func() (string, error) { return "172.20.0.1", nil }
	for _, route := range []string{"mesh", "cloudbox"} {
		for _, key := range []string{"bpeer_first", "bpeer_rotated"} {
			wslInterop = func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Join(args, " ") != "llm env --from dragon --json --bind-for-wsl 172.20.0.1" {
					t.Fatalf("interop args: %v", args)
				}
				base := "http://172.20.0.1:32000/v1"
				if route == "cloudbox" {
					base = "https://example.test/k/" + key + "/v1"
				}
				return json.Marshal(map[string]any{"via": route, "env": map[string]string{"OPENAI_BASE_URL": base, "OPENAI_API_KEY": key}})
			}
			m, ok, err := ResolveDoorModel(context.Background(), "codex-gpt6-sol@dragon")
			if err != nil || !ok || m.Key != key || m.BaseURL == "" {
				t.Fatalf("WSL %s: ok=%v err=%v", route, ok, err)
			}
		}
	}
}

func TestDoorModelRefusesMissingOrInvalidSource(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	useFakeOutpost(t)
	for _, name := range []string{"m@dragon", "dragon-door-m", "m@../dragon", "@dragon", "m@"} {
		_, ok, err := ResolveDoorModel(context.Background(), name)
		if !ok || err == nil {
			t.Fatalf("%q fell through: ok=%v err=%v", name, ok, err)
		}
	}
	for _, name := range []string{"codex-gpt6-sol", "qwen3:8b", "door-codex-gpt6-sol"} {
		_, ok, err := ResolveDoorModel(context.Background(), name)
		if ok || err != nil {
			t.Fatalf("%q intercepted: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestDoorModelMeshUsesCurrentGrant(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	f := useFakeOutpost(t)
	f.consumes = "llm <- peer PEERID-DOOR -> 127.0.0.1:40123\n"
	g := testGrant()
	for _, key := range []string{"bpeer_first", "bpeer_rotated"} {
		g.Credential = key
		if err := SaveDoorGrant(g); err != nil {
			t.Fatal(err)
		}
		m, ok, err := ResolveDoorModel(context.Background(), "qwen3:8b@"+g.Door)
		if err != nil || !ok || m.Key != key || m.BaseURL != "http://127.0.0.1:40123/v1" || m.ModelID != "qwen3:8b" {
			t.Fatalf("mesh resolution: ok=%v err=%v", ok, err)
		}
	}
	if f.count("mesh consume add") != 0 {
		t.Fatal("did not reuse persistent forward")
	}
}
