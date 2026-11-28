package broker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/broker/door"
)

// fakeDoor is an OpenAI-compatible door answering one fixed content.
type fakeDoor struct {
	*httptest.Server
	content, refusal string
	path, auth       string
	body             map[string]any
}

func newFakeDoor(t *testing.T, content string) *fakeDoor {
	return startFakeDoor(t, content, httptest.NewServer)
}

func startFakeDoor(t *testing.T, content string, start func(http.Handler) *httptest.Server) *fakeDoor {
	t.Helper()
	d := &fakeDoor{content: content}
	d.Server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.path, d.auth = r.URL.Path, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		d.body = nil
		_ = json.Unmarshal(data, &d.body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   d.body["model"],
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": d.content, "refusal": d.refusal}}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	t.Cleanup(d.Close)
	return d
}

// useLocalDoor makes d the local door of a private BASHY_HOME.
func useLocalDoor(t *testing.T, d *fakeDoor) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "")
	t.Setenv("BASHY_AGENTIC", "")
	_, port, err := net.SplitHostPort(d.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(door.PortEnv, port)
}

func callJSON(t *testing.T, args ...string) (CallResult, error) {
	t.Helper()
	out, err := runShareLLM(t, "prompt from stdin", append([]string{"call", "--json"}, args...)...)
	var res CallResult
	if strings.TrimSpace(out) != "" {
		if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
			t.Fatalf("not an envelope: %v\n%s", jerr, out)
		}
	}
	return res, err
}

func TestLLMCallLocalDoorReturnsTextAndUsage(t *testing.T) {
	d := newFakeDoor(t, "hello there")
	useLocalDoor(t, d)
	res, err := callJSON(t, "--band", "L4", "--system", "be brief", "--max-tokens", "64", "say", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.SchemaVersion != CallSchemaVersion || res.Model != "L4" || res.Text != "hello there" || res.JSON != nil {
		t.Fatalf("envelope: %+v", res)
	}
	if res.Usage == nil || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	token, _ := door.Token()
	if d.path != "/v1/chat/completions" || d.auth != "Bearer "+token {
		t.Fatalf("route %q auth set=%v", d.path, d.auth != "")
	}
	messages, _ := d.body["messages"].([]any)
	if len(messages) != 2 || d.body["stream"] != false || d.body["max_tokens"] != float64(64) {
		t.Fatalf("request: %v", d.body)
	}
	if user := messages[1].(map[string]any); user["content"] != "say hello" {
		t.Fatalf("prompt: %v", user)
	}

	// The prompt may come from standard input; plain output is the text.
	out, err := runShareLLM(t, "prompt from stdin", "call", "--model", "qwen3:8b", "-")
	if err != nil || out != "hello there\n" {
		t.Fatalf("plain: %q %v", out, err)
	}
	if got := d.body["messages"].([]any)[0].(map[string]any)["content"]; got != "prompt from stdin" {
		t.Fatalf("stdin prompt: %v", got)
	}
	if _, err := runShareLLM(t, "", "call", "hello"); err == nil {
		t.Fatal("a call naming no model or band must be refused")
	}
}

func TestLLMCallSchemaParsesOrRefusesTyped(t *testing.T) {
	const schema = `{"type":"object","required":["n"],"properties":{"n":{"type":"integer"}},"additionalProperties":false}`
	d := newFakeDoor(t, "```json\n{\"n\": 3}\n```")
	useLocalDoor(t, d)
	res, err := callJSON(t, "--band", "L4", "--schema", schema, "count")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON) != `{"n":3}` || res.Error != nil || res.Usage == nil {
		t.Fatalf("json: %s %+v", res.JSON, res)
	}
	if format, _ := d.body["response_format"].(map[string]any); format["type"] != "json_schema" {
		t.Fatalf("no response_format: %v", d.body)
	}
	for name, tc := range map[string]struct{ content, refusal, kind string }{
		"not json":      {"three", "", "schema"},
		"wrong shape":   {`{"n":"three"}`, "", "schema"},
		"model refusal": {"", "I cannot help with that", "refusal"},
	} {
		d.content, d.refusal = tc.content, tc.refusal
		res, err := callJSON(t, "--band", "L4", "--schema", schema, "count")
		var typed *CallError
		if err == nil || res.Error == nil || res.Error.Kind != tc.kind || res.JSON != nil {
			t.Fatalf("%s: want a typed %s refusal, got %+v err=%v", name, tc.kind, res, err)
		}
		if e, ok := err.(*CallError); ok {
			typed = e
		}
		if typed == nil || typed.Kind != tc.kind {
			t.Fatalf("%s: error is not typed: %v", name, err)
		}
	}
	if _, err := runShareLLM(t, "", "call", "--band", "L4", "--schema", `{"type":`, "x"); err == nil {
		t.Fatal("an invalid schema must be refused before the call")
	}
}

func TestLLMCallRoutesModelAtDoorToTheNamedDoor(t *testing.T) {
	local := newFakeDoor(t, "local")
	// A grant's cloudbox route is https only.
	remote := startFakeDoor(t, "from dragon", httptest.NewTLSServer)
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = remote.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	useLocalDoor(t, local)
	useFakeOutpost(t)
	g := DoorGrant{Schema: GrantSchemaVersion, Door: "dragon", Peer: "elephant", CloudboxURL: remote.URL + "/app/llm", Credential: "bpeer_first"}
	data, _ := json.Marshal(g)
	if out, err := runShareLLM(t, string(data), "door", "import"); err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	res, err := callJSON(t, "--model", "qwen3:8b@dragon", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "from dragon" || res.Model != "qwen3:8b" || local.path != "" {
		t.Fatalf("not routed to the named door: %+v local=%q", res, local.path)
	}
	if remote.body["model"] != "qwen3:8b" || !strings.HasSuffix(remote.path, "/v1/chat/completions") || remote.auth != "Bearer bpeer_first" {
		t.Fatalf("door request: path=%q model=%v", remote.path, remote.body["model"])
	}
	// An unknown door is a typed door failure, never a fallback to the local door.
	res, err = callJSON(t, "--model", "qwen3:8b@nowhere", "hi")
	if err == nil || res.Error == nil || res.Error.Kind != "door" || local.path != "" {
		t.Fatalf("unknown door: %+v err=%v local=%q", res, err, local.path)
	}
}

func TestLLMTypedToolListsWithStableDigestAndCalls(t *testing.T) {
	d := newFakeDoor(t, "typed answer")
	useLocalDoor(t, d)
	ctx := context.Background()
	build := func() *mcpsdk.Server {
		srv, err := mcp.BuildServer("test", "0", mcp.Options{
			Policy:     &mcp.Policy{Allow: map[string]bool{"spend": true}},
			Registered: func() []mcp.RegisteredCommand { return []mcp.RegisteredCommand{MCPCommand()} },
		})
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	srv := build()
	first, err := mcp.ToolsSHA256(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := mcp.ToolsSHA256(ctx, build()); first == "" || first != second {
		t.Fatalf("digest not stable: %q %q", first, second)
	}
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var def *mcpsdk.Tool
	for _, tl := range listed.Tools {
		if tl.Name == "llm" {
			def = tl
		}
	}
	if def == nil {
		t.Fatal("llm is not in the tool list")
	}
	raw, _ := json.Marshal(def.InputSchema)
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	_ = json.Unmarshal(raw, &schema)
	if schema.Properties["prompt"]["type"] != "string" || schema.Properties["max-tokens"]["type"] != "integer" || len(schema.Required) != 2 {
		t.Fatalf("llm is not typed: %s", raw)
	}
	if !strings.Contains(def.Description, CallSchemaVersion) || !strings.Contains(def.Description, "Unit: token") {
		t.Fatalf("definition lacks output shape or unit: %s", def.Description)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "llm", Arguments: map[string]any{"prompt": "-starts with a dash", "model": "L4"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	var out mcp.RunToolOutput
	_ = json.Unmarshal(structured, &out)
	var env CallResult
	if err := json.Unmarshal([]byte(out.Stdout), &env); err != nil || env.Text != "typed answer" || env.Usage == nil {
		t.Fatalf("tool output: %s (%v)", structured, err)
	}
}
