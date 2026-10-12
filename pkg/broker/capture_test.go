package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureLevel(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"absent", `{"choices":[{"message":{"content":"hi"}}]}`, "eval"},
		{"logprobs alone", `{"choices":[{"logprobs":{"content":[{"token":"hi","logprob":-1}]}}]}`, "eval"},
		{"both", `{"choices":[{"logprobs":{"content":[{"token":"hi","token_id":42,"logprob":-1}]}}]}`, "train"},
		{"vllm", `{"prompt_token_ids":[1],"logprobs":[-1]}`, "train"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureLevel([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCaptureDoor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	session := "s-abcdef123456"
	if err := writeCaptureSession(session, "episode-test"); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Capture = true })
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	secret := "sk-abcdefghijklmnop1234"
	request := map[string]any{"model": "L4", "messages": []any{map[string]any{"role": "user", "content": strings.Join([]string{secret, host, user}, " ")}}}
	resp, _ := h.do(http.MethodPost, "/s/"+session+"/v1/chat/completions", request, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	path := filepath.Join(home, "cligw", "capture", session, "turns.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("got %d lines", n)
	}
	var row captureTurn
	if err = json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	if row.Session != session || row.Episode != "episode-test" || row.Dialect != "openai" || row.Status != 200 || row.CaptureLevel != "eval" || row.Routed == "" {
		t.Fatalf("bad capture: %+v", row)
	}
	for _, s := range []string{secret, host, user, "Authorization", testToken} {
		if s != "" && strings.Contains(string(data), s) {
			t.Fatalf("capture contains sensitive %q", s)
		}
	}
	if row.Usage == nil || row.Usage.PromptTokens != 2 {
		t.Fatalf("usage: %+v", row.Usage)
	}
}

func TestCaptureOffAndNoEpisode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	h := newHarness(t, nil)
	h.do(http.MethodPost, "/s/s-abcdef123456/v1/chat/completions", map[string]any{"model": "L4", "messages": []any{}}, nil)
	if _, err := os.Stat(filepath.Join(home, "cligw", "capture")); !os.IsNotExist(err) {
		t.Fatalf("capture off created directory: %v", err)
	}
	h2 := newHarness(t, func(o *Options) { o.Capture = true })
	h2.do(http.MethodPost, "/s/s-fedcba123456/v1/chat/completions", map[string]any{"model": "L4", "messages": []any{}}, nil)
	turns, err := readCaptureTurns("s-fedcba123456")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Episode != "" {
		t.Fatalf("turns: %+v", turns)
	}
}

func TestCaptureDialects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	h := newHarness(t, func(o *Options) { o.Capture = true })
	for _, tc := range []struct{ path, model, dialect string }{
		{"/v1/responses", "L4", "responses"},
		{"/anthropic/v1/messages", "L4", "anthropic"},
		{"/api/chat", "llama3.2:3b", "ollama"},
		{"/api/generate", "llama3.2:3b", "ollama"},
	} {
		h.do(http.MethodPost, "/s/s-112233445566"+tc.path, map[string]any{"model": tc.model, "messages": []any{}, "prompt": "hello"}, nil)
	}
	var turns []captureTurn
	var err error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		turns, err = readCaptureTurns("s-112233445566")
		if err == nil && len(turns) == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 {
		t.Fatalf("got %d turns: %+v", len(turns), turns)
	}
	for i, want := range []string{"responses", "anthropic", "ollama", "ollama"} {
		if turns[i].Dialect != want {
			t.Fatalf("turn %d dialect %q", i, turns[i].Dialect)
		}
	}
}

func TestCaptureStreamingFlush(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	h := newHarness(t, func(o *Options) { o.Capture = true })
	continued := make(chan struct{})
	h.eng.chatFn = func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"first\":true}\n\n")
		w.(http.Flusher).Flush()
		<-continued
		io.WriteString(w, "data: {\"last\":true}\n\n")
	}
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/s/s-aabbccddeeff/v1/chat/completions", strings.NewReader(`{"model":"llama3.2:3b","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(continued)
		t.Fatal(err)
	}
	first := make([]byte, len("data: {\"first\":true}\n\n"))
	done := make(chan error, 1)
	go func() { _, e := io.ReadFull(resp.Body, first); done <- e }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(continued)
		t.Fatal("first chunk did not flush")
	}
	close(continued)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	turns, err := readCaptureTurns("s-aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || !strings.Contains(turns[0].Response, "last") {
		t.Fatalf("stream capture: %+v", turns)
	}
}
