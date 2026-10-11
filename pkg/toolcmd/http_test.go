package toolcmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestHTTPCommandsUseJSONAndKeepSecretsOutOfResults(t *testing.T) {
	const token = "secret-token-never-report"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen[r.URL.Path] = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	for _, name := range []string{"bots", "send", "approve", "usage"} {
		cmd := fleet.ToolCommand{Name: name, Mode: fleet.ToolCommandHTTP, HTTP: &fleet.ToolCommandHTTPAction{
			Method: "POST", URL: server.URL, Path: "/" + name, TokenFile: tokenFile,
			Body: map[string]any{"text": "say {args}", "nested": map[string]any{"again": "{args}"}},
		}}
		arg := "quote: \"x\"\nnext line"
		res, err := Run(context.Background(), fleet.Tool{Name: "codync"}, cmd, arg, Options{})
		if err != nil || res.Outcome != OutcomeSuccess || res.HTTPStatus != http.StatusOK || res.Text != `{"ok":true}` {
			t.Fatalf("%s: result=%+v err=%v", name, res, err)
		}
		if strings.Contains(res.Error+res.Text, token) {
			t.Fatalf("%s leaked token in result: %+v", name, res)
		}
		if got := seen["/"+name]["text"]; got != "say "+arg {
			t.Fatalf("%s JSON body text = %#v", name, got)
		}
		if got := seen["/"+name]["nested"].(map[string]any)["again"]; got != arg {
			t.Fatalf("%s nested JSON body = %#v", name, got)
		}
	}
}

func TestHTTPCommandFailuresDoNotLeakToken(t *testing.T) {
	const token = "very-secret-token"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, token, http.StatusBadGateway) }))
	defer server.Close()
	cmd := fleet.ToolCommand{Name: "send", Mode: fleet.ToolCommandHTTP, HTTP: &fleet.ToolCommandHTTPAction{Method: "POST", URL: server.URL, TokenFile: tokenFile}}
	res, err := Run(context.Background(), fleet.Tool{Name: "codync"}, cmd, "", Options{})
	if err == nil || res.Outcome != OutcomeError || res.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if strings.Contains(err.Error()+res.Error+res.Text, token) {
		t.Fatalf("secret leaked: result=%+v err=%v", res, err)
	}

	missing := cmd
	missing.HTTP = &fleet.ToolCommandHTTPAction{Method: "POST", URL: server.URL, TokenFile: filepath.Join(t.TempDir(), "missing")}
	res, err = Run(context.Background(), fleet.Tool{Name: "codync"}, missing, "", Options{})
	if err == nil || res.Outcome != OutcomeError || strings.Contains(err.Error()+res.Error, token) {
		t.Fatalf("missing token: result=%+v err=%v", res, err)
	}
}

func TestHTTPCommandResolvesTokenRef(t *testing.T) {
	const token = "ref-secret"
	t.Setenv("CODYNC_TOKEN", token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmd := fleet.ToolCommand{Name: "usage", Mode: fleet.ToolCommandHTTP, HTTP: &fleet.ToolCommandHTTPAction{Method: "GET", URL: server.URL, TokenRef: "codync"}}
	res, err := Run(context.Background(), fleet.Tool{Name: "codync"}, cmd, "", Options{})
	if err != nil || res.Outcome != OutcomeSuccess || res.HTTPStatus != http.StatusNoContent {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}
