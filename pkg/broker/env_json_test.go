package broker

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// deadDoorPort points the door port at a closed one: env --sticky asks the door
// who owns a key and must never reach a real door running on this host.
func deadDoorPort(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	t.Setenv(door.PortEnv, port)
}

// runEnv runs `llm env` with args against an isolated BASHY_HOME and returns
// its stdout. The door is never contacted: env only mints the owner token.
func runEnv(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newEnvCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("llm env %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// `llm env --json` is the release-bar probe for the llm verb: the schema
// version must be IN the envelope, and the same five variables the shell form
// exports must be under env, so a client that reads JSON needs no second call.
func TestEnvJSONEnvelope(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "sess-1")
	deadDoorPort(t)

	var got struct {
		SchemaVersion string            `json:"schema_version"`
		BaseURL       string            `json:"base_url"`
		Session       string            `json:"session"`
		Sticky        string            `json:"sticky"`
		Env           map[string]string `json:"env"`
	}
	raw := runEnv(t, "--json", "--sticky", "k1")
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not a single JSON value: %v\n%s", err, raw)
	}
	// The literal is pinned on purpose: renaming the constant must not move
	// the envelope the release bar probes.
	if got.SchemaVersion != "bashy-llm-env-v1" {
		t.Errorf("schema_version = %q, want %q", got.SchemaVersion, "bashy-llm-env-v1")
	}
	if EnvSchemaVersion != "bashy-llm-env-v1" {
		t.Errorf("EnvSchemaVersion = %q, want %q", EnvSchemaVersion, "bashy-llm-env-v1")
	}
	if got.BaseURL != door.BaseURL() {
		t.Errorf("base_url = %q, want %q", got.BaseURL, door.BaseURL())
	}
	if got.Session != "sess-1" || got.Sticky != "k1" {
		t.Errorf("session/sticky = %q/%q, want sess-1/k1", got.Session, got.Sticky)
	}
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OPENAI_BASE_URL":    door.BaseURL() + "/s/sess-1/sticky/k1/v1",
		"OPENAI_API_KEY":     token,
		"ANTHROPIC_BASE_URL": door.BaseURL() + "/s/sess-1/sticky/k1/anthropic",
		"ANTHROPIC_API_KEY":  token,
		"OLLAMA_HOST":        door.BaseURL() + "/k/" + token + "/s/sess-1/sticky/k1",
	}
	if len(got.Env) != len(want) {
		t.Errorf("env has %d keys, want %d: %v", len(got.Env), len(want), got.Env)
	}
	for k, v := range want {
		if got.Env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got.Env[k], v)
		}
	}
}

// The shell form is unchanged by the flag: same values, same export line, so
// `eval "$(bashy llm env)"` keeps working for every existing reader.
func TestEnvShellFormMatchesJSON(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "")
	t.Setenv(door.PortEnv, "")

	shell := runEnv(t)
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(runEnv(t, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(shell), "\n")
	order := []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "OLLAMA_HOST"}
	if len(lines) != len(order)+2 {
		t.Fatalf("shell form has %d lines, want %d:\n%s", len(lines), len(order)+2, shell)
	}
	for i, k := range order {
		if want := k + "=" + got.Env[k]; lines[i] != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	if want := "export " + strings.Join(order, " "); lines[len(order)] != want {
		t.Errorf("last line = %q, want %q", lines[len(order)], want)
	}
	if want := "# OpenAI routes: /v1/chat/completions /v1/responses /v1/models"; lines[len(order)+1] != want {
		t.Errorf("route discovery = %q, want %q", lines[len(order)+1], want)
	}
	if strings.Contains(shell, "schema_version") {
		t.Errorf("shell form must not carry the JSON envelope:\n%s", shell)
	}
}

// A bad sticky key is refused in JSON mode too, before anything is printed.
func TestEnvJSONRejectsBadStickyKey(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	cmd := newEnvCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json", "--sticky", "not a key!"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid sticky key, got output:\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("nothing must be printed on refusal, got:\n%s", out.String())
	}
}
