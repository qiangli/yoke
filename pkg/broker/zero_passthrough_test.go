package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"gopkg.in/yaml.v3"
)

type zeroPassthroughCLI struct {
	script, endpoint string
}

func (c zeroPassthroughCLI) Token() string { return "cli-token" }
func (c zeroPassthroughCLI) ResolveAgent(context.Context, string, string) (AgentInfo, error) {
	return AgentInfo{Name: "zero-bedrock-glm-5", Tool: "zero-ycode", Model: "bedrock-glm-5", VendorModel: "fake-model"}, nil
}
func (c zeroPassthroughCLI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cmd := exec.CommandContext(r.Context(), "bashy", c.script, "hello")
	cmd.Env = append(os.Environ(), "OPENAI_BASE_URL="+c.endpoint, "OPENAI_API_KEY=fake-key")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, err.Error()+": "+string(raw), http.StatusBadGateway)
		return
	}
	var event struct {
		Result   string `json:"result"`
		Response struct {
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
				Cached int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &event); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := cligw.Usage{InputTokens: int64(event.Response.Usage.Input), OutputTokens: int64(event.Response.Usage.Output), CachedInputTokens: int64(event.Response.Usage.Cached), TotalTokens: int64(event.Response.Usage.Input + event.Response.Usage.Output)}
	contentType, body := cligw.EncodeChat(openai.ChatRequest{Model: "zero-bedrock-glm-5"}, event.Result, "stop", "chatcmpl-fake", nil, u)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Bashy-Upstream-Model", "fake-model")
	w.Write(body)
}

// One endpoint call exercises the zero harness, its usage event, cligw's
// response shape, and the broker's persisted audit record.
func TestZeroPassthroughAudit(t *testing.T) {
	if _, err := exec.LookPath("bashy"); err != nil {
		t.Skip("bashy ycode is not installed")
	}
	var requests int
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "fake-model" {
			t.Errorf("provider request model = %q, error %v", request.Model, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"fake","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"PASSTHROUGH_OK"}}]}`,
			`{"id":"fake","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"fake","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":17,"completion_tokens":3,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":5}}}`,
		} {
			w.Write([]byte("data: " + chunk + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer endpoint.Close()
	data, err := os.ReadFile(filepath.Join("..", "fleet", "baseline", "tools", "zero-ycode.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tool struct {
		CLI struct {
			Launch struct {
				Setup string `yaml:"setup"`
			} `yaml:"launch"`
		} `yaml:"cli"`
	}
	if err := yaml.Unmarshal(data, &tool); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	setup := strings.NewReplacer("{state_dir}", "'"+state+"'", "{model}", "'fake-model'").Replace(tool.CLI.Launch.Setup)
	if out, err := exec.Command("bashy", "-c", setup).CombinedOutput(); err != nil {
		t.Fatalf("zero harness setup: %v: %s", err, out)
	}
	stateDir := t.TempDir()
	cli := zeroPassthroughCLI{script: filepath.Join(state, "run.sh"), endpoint: endpoint.URL + "/v1"}
	b, err := New(context.Background(), Options{CLI: cli, Token: testToken, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"zero-bedrock-glm-5","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "PASSTHROUGH_OK") || requests != 1 {
		t.Fatalf("passthrough status=%d body=%s provider requests=%d", w.Code, w.Body.String(), requests)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(bytes.TrimSpace(raw), &record); err != nil {
		t.Fatal(err)
	}
	if record.PromptTok != 17 || record.OutputTok != 3 || record.CachedTok != 5 || record.UpstreamModel != "fake-model" {
		t.Fatalf("audit usage = %+v", record)
	}
}
