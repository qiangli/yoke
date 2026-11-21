package cligw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

var bashyGateTool = openai.Tool{
	Type: "function",
	Function: openai.ToolFunction{
		Name: "bashy",
		Parameters: json.RawMessage(`{
			"type":"object",
			"additionalProperties":false,
			"properties":{
				"script":{"type":"string"},
				"timeout_ms":{"type":"integer","minimum":1},
				"digest":{"type":"string"}
			},
			"required":["script"]
		}`),
	},
}

func TestRecoverXMLToolCalls_ShapesAndValidation(t *testing.T) {
	tools := []openai.Tool{bashyGateTool}
	cases := []struct {
		name       string
		answer     string
		tools      []openai.Tool
		toolChoice json.RawMessage
		wantOK     bool
		wantCalls  int
		wantScript string
		wantMS     int64
	}{
		{
			name:       "bare invoke tag from notepad/viewer/chart",
			answer:     "<invoke name=\"bashy\">\n<parameter name=\"script\">bashy shape status viewer --user device:m_alice --json</parameter>\n</invoke>",
			tools:      tools,
			wantOK:     true,
			wantCalls:  1,
			wantScript: "bashy shape status viewer --user device:m_alice --json",
		},
		{
			name:       "prose plus function_calls wrapper with typed int and shell redirects",
			answer:     "Let me check the current state.\n\n<function_calls>\n<invoke name=\"bashy\">\n<parameter name=\"script\">bashy shape status notepad --user device:m_alice --json && grep -q '<h1>' index.html</parameter>\n<parameter name=\"timeout_ms\">5000</parameter>\n</invoke>\n</function_calls>",
			tools:      tools,
			wantOK:     true,
			wantCalls:  1,
			wantScript: "bashy shape status notepad --user device:m_alice --json && grep -q '<h1>' index.html",
			wantMS:     5000,
		},
		{
			name:       "antml namespace and xml entities",
			answer:     "<antml:function_calls>\n<antml:invoke name='bashy'>\n<antml:parameter name='script'>echo &quot;hi&quot; &amp;&amp; test 1 &lt; 2</antml:parameter>\n</antml:invoke>\n</antml:function_calls>",
			tools:      tools,
			wantOK:     true,
			wantCalls:  1,
			wantScript: `echo "hi" && test 1 < 2`,
		},
		{
			name:      "multi-invoke batch",
			answer:    "<function_calls>\n<invoke name=\"bashy\">\n<parameter name=\"script\">echo one</parameter>\n</invoke>\n<invoke name=\"bashy\">\n<parameter name=\"script\">echo two</parameter>\n</invoke>\n</function_calls>",
			tools:     tools,
			wantOK:    true,
			wantCalls: 2,
		},
		{
			name:   "refuse undeclared tool",
			answer: "<invoke name=\"rm\">\n<parameter name=\"script\">rm -rf /</parameter>\n</invoke>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse missing required script",
			answer: "<invoke name=\"bashy\">\n<parameter name=\"timeout_ms\">5000</parameter>\n</invoke>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse wrong typed parameter",
			answer: "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n<parameter name=\"timeout_ms\">notanint</parameter>\n</invoke>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse unknown additional property",
			answer: "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n<parameter name=\"shell\">zsh</parameter>\n</invoke>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse trailing prose after invoke",
			answer: "Example:\n<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>\nDo not execute examples.",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse inline prose mention",
			answer: `Do not emit <invoke name="bashy"><parameter name="script">ls</parameter></invoke> here.`,
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse unclosed invoke",
			answer: "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:   "refuse ordinary xml or html text",
			answer: "<div class=\"card\"><span>Hello</span></div>",
			tools:  tools,
			wantOK: false,
		},
		{
			name:       "refuse when tool_choice is none",
			answer:     "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>",
			tools:      tools,
			toolChoice: json.RawMessage(`"none"`),
			wantOK:     false,
		},
		{
			name:       "refuse when tool_choice pins another tool",
			answer:     "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>",
			tools:      []openai.Tool{bashyGateTool, {Type: "function", Function: openai.ToolFunction{Name: "other", Parameters: json.RawMessage(`{"type":"object"}`)}}},
			toolChoice: json.RawMessage(`{"type":"function","function":{"name":"other"}}`),
			wantOK:     false,
		},
		{
			name:   "refuse when no tools offered",
			answer: "<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>",
			tools:  nil,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := recoverXMLToolCalls(tc.answer, tc.tools, tc.toolChoice)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v (got=%q), want %v", ok, got, tc.wantOK)
			}
			if !ok {
				return
			}
			var env struct {
				ToolCalls []struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"tool_calls"`
			}
			if err := json.Unmarshal([]byte(got), &env); err != nil {
				t.Fatalf("invalid envelope %q: %v", got, err)
			}
			if len(env.ToolCalls) != tc.wantCalls {
				t.Fatalf("calls=%d, want %d: %s", len(env.ToolCalls), tc.wantCalls, got)
			}
			if tc.wantScript != "" {
				var args struct {
					Script    string `json:"script"`
					TimeoutMS int64  `json:"timeout_ms"`
				}
				if err := json.Unmarshal(env.ToolCalls[0].Arguments, &args); err != nil {
					t.Fatal(err)
				}
				if args.Script != tc.wantScript || args.TimeoutMS != tc.wantMS {
					t.Fatalf("args=%+v, want script=%q timeout_ms=%d", args, tc.wantScript, tc.wantMS)
				}
			}
		})
	}
}

// Offline replay of sanitized Claude CLI model-door turns across both OpenAI
// (/v1/chat/completions) and Anthropic (/anthropic/v1/messages), streaming and
// non-streaming:
//   - native JSON {"tool_calls":[...]} envelope (Hello World / notepad step 1)
//   - terminal <invoke ...> XML text (notepad step 2 / viewer / chart step 1)
//   - ordinary final text (must remain plain text)
//   - policy-rejected XML call (unknown tool / missing arg -> stays plain text)
func TestClaudeDoorOfflineReplayToolFramingAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fixtureLines []string
		toolChoice   any
		wantToolCall bool
		wantScript   string
		wantText     string
	}{
		{
			name: "native structured envelope",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"tool_calls\":[{\"name\":\"bashy\",\"arguments\":{\"script\":\"bashy shape status hello-world --user device:m_alice --json\"}}]}"}}}`,
				`{"type":"result","is_error":false,"result":"{\"tool_calls\":[{\"name\":\"bashy\",\"arguments\":{\"script\":\"bashy shape status hello-world --user device:m_alice --json\"}}]}","usage":{"input_tokens":10,"output_tokens":5}}`,
			},
			wantToolCall: true,
			wantScript:   "bashy shape status hello-world --user device:m_alice --json",
		},
		{
			name: "terminal invoke xml text",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Checking status.\n<invoke name=\"bashy\">\n"}}}`,
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"<parameter name=\"script\">bashy shape status notepad --user device:m_alice --json</parameter>\n</invoke>"}}}`,
				`{"type":"result","is_error":false,"result":"Checking status.\n<invoke name=\"bashy\">\n<parameter name=\"script\">bashy shape status notepad --user device:m_alice --json</parameter>\n</invoke>","usage":{"input_tokens":12,"output_tokens":8}}`,
			},
			wantToolCall: true,
			wantScript:   "bashy shape status notepad --user device:m_alice --json",
		},
		{
			name: "ordinary final text preserved",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"E2E_NOTEPAD_PASS"}}}`,
				`{"type":"result","is_error":false,"result":"E2E_NOTEPAD_PASS","usage":{"input_tokens":8,"output_tokens":3}}`,
			},
			wantToolCall: false,
			wantText:     "E2E_NOTEPAD_PASS",
		},
		{
			name: "undeclared tool invoke stays non-executable text",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"<invoke name=\"shell\">\n<parameter name=\"script\">rm -rf /</parameter>\n</invoke>"}}}`,
				`{"type":"result","is_error":false,"result":"<invoke name=\"shell\">\n<parameter name=\"script\">rm -rf /</parameter>\n</invoke>","usage":{"input_tokens":8,"output_tokens":4}}`,
			},
			wantToolCall: false,
			wantText:     `name=\"shell\"`,
		},
		{
			name: "wrong typed parameter stays non-executable text",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n<parameter name=\"timeout_ms\">bad</parameter>\n</invoke>"}}}`,
				`{"type":"result","is_error":false,"result":"<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n<parameter name=\"timeout_ms\">bad</parameter>\n</invoke>","usage":{"input_tokens":8,"output_tokens":4}}`,
			},
			wantToolCall: false,
			wantText:     `timeout_ms`,
		},
		{
			name: "tool_choice none preserves text without executing invoke",
			fixtureLines: []string{
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>"}}}`,
				`{"type":"result","is_error":false,"result":"<invoke name=\"bashy\">\n<parameter name=\"script\">ls</parameter>\n</invoke>","usage":{"input_tokens":8,"output_tokens":4}}`,
			},
			toolChoice:   "none",
			wantToolCall: false,
			wantText:     `name=\"bashy\"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixturePath := t.TempDir() + "/events.jsonl"
			if err := os.WriteFile(fixturePath, []byte(strings.Join(tc.fixtureLines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BASHY_HOME", t.TempDir())
			t.Setenv("CLIGW_MUSE_FIXTURE", fixturePath)
			cat := installClaudeFixtureFleet(t)
			if w, err := NewWorker(t.Context(), "claude-sonnet"); err != nil {
				t.Fatalf("NewWorker precheck: %v", err)
			} else {
				_ = w.Close()
			}
			srv, err := NewServer(ServerOptions{
				Catalog: cat,
				Quota:   &fakeQuota{headroom: map[string]float64{"sonnet-x": 0.9}, refused: map[string]string{}},
				Breaker: sched.NewBreaker(),
				Pool:    PoolConfig{StartServers: 0, MaxWorkers: 2, MaxSpare: 1, IdleTTL: time.Minute},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			httpSrv := httptest.NewServer(srv.Handler())
			t.Cleanup(httpSrv.Close)

			// 1. OpenAI non-streaming + streaming
			for _, stream := range []bool{false, true} {
				payload := map[string]any{
					"model":    "claude-sonnet",
					"stream":   stream,
					"messages": []any{map[string]any{"role": "user", "content": "status"}},
					"tools":    []any{bashyGateTool},
				}
				if tc.toolChoice != nil {
					payload["tool_choice"] = tc.toolChoice
				}
				bodyBytes, _ := json.Marshal(payload)
				req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/chat/completions", strings.NewReader(string(bodyBytes)))
				req.Header.Set("Authorization", "Bearer "+srv.Token())
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpSrv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("openai stream=%v status=%s body=%s", stream, resp.Status, raw)
				}
				got := string(raw)
				if tc.wantToolCall {
					if !strings.Contains(got, `"tool_calls"`) || !strings.Contains(got, `"name":"bashy"`) || !strings.Contains(got, tc.wantScript) || !strings.Contains(got, `"finish_reason":"tool_calls"`) {
						t.Fatalf("openai stream=%v expected structured bashy tool_call (%q), got:\n%s", stream, tc.wantScript, got)
					}
				} else {
					if strings.Contains(got, `"tool_calls"`) || !strings.Contains(got, tc.wantText) {
						t.Fatalf("openai stream=%v expected plain text %q without tool_calls, got:\n%s", stream, tc.wantText, got)
					}
				}
			}

			// 2. Anthropic /anthropic/v1/messages non-streaming + streaming
			for _, stream := range []bool{false, true} {
				payload := map[string]any{
					"model":      "claude-sonnet",
					"max_tokens": 1024,
					"stream":     stream,
					"messages":   []any{map[string]any{"role": "user", "content": "status"}},
					"tools": []any{map[string]any{
						"name":         "bashy",
						"description":  "Run bashy",
						"input_schema": bashyGateTool.Function.Parameters,
					}},
				}
				if tc.toolChoice != nil {
					payload["tool_choice"] = map[string]any{"type": tc.toolChoice}
				}
				bodyBytes, _ := json.Marshal(payload)
				req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/anthropic/v1/messages", strings.NewReader(string(bodyBytes)))
				req.Header.Set("x-api-key", srv.Token())
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpSrv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("anthropic stream=%v status=%s body=%s", stream, resp.Status, raw)
				}
				got := string(raw)
				if tc.wantToolCall {
					if !strings.Contains(got, `"tool_use"`) || !strings.Contains(got, `"name":"bashy"`) || !strings.Contains(got, tc.wantScript) {
						t.Fatalf("anthropic stream=%v expected tool_use (%q), got:\n%s", stream, tc.wantScript, got)
					}
				} else {
					if strings.Contains(got, `"tool_use"`) || !strings.Contains(got, tc.wantText) {
						t.Fatalf("anthropic stream=%v expected plain text %q without tool_use, got:\n%s", stream, tc.wantText, got)
					}
				}
			}
		})
	}
}

func installClaudeFixtureFleet(t *testing.T) *FleetCatalog {
	t.Helper()
	t.Setenv("CLIGW_TEST_HELPER", "1")
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	seed := seededGatewayLaunch(t, "claude")
	launch := fleet.ToolLaunch{
		Exec:               os.Args[0] + " -test.run=TestCLIHelper -- response-fixture {model} {prompt}",
		Warm:               string(WarmCold),
		StdinFormat:        seed.StdinFormat,
		NativeSystemPrompt: seed.NativeSystemPrompt,
		Decoder:            seed.Decoder,
		EventsStdout:       "--events-json",
		EventsDone:         fleet.EventsDone{Field: "type", Values: []string{"result"}},
		EventsOutcome:      fleet.EventsOutcome{Path: "is_error", OK: []string{"false"}},
	}
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "sonnet-x", Band: 4, Kind: fleet.ModelKindSubscription, Provider: "anthropic", Domain: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "claude-sonnet", Tool: "claude", Model: "sonnet-x"}); err != nil {
		t.Fatal(err)
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
	return NewFleetCatalog(cat)
}
