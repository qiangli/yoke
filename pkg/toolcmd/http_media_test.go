package toolcmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTypedHTTPMediaInputsAndPredictionPathWithoutNetwork(t *testing.T) {
	old := httpClient
	defer func() { httpClient = old }()
	calls := 0
	httpClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "api.example.test" || r.Header.Get("Authorization") != "Bearer fixture" {
				t.Errorf("request target/auth: %s", r.URL.Host)
			}
			if calls == 1 {
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if got["input"].(map[string]any)["prompt"] != "sing" {
					t.Errorf("body = %#v", got)
				}
			} else if r.URL.EscapedPath() != "/v1/predictions/id%2Fone" || r.ContentLength != 0 {
				t.Errorf("GET path/body = %s %v", r.URL.EscapedPath(), r.Body)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"succeeded"}`)), Header: make(http.Header)}, nil
		})}
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(input, []byte(`{"prompt":"sing"}`), 0600); err != nil {
		t.Fatal(err)
	}
	create := fleet.ToolCommand{Name: "create", Mode: fleet.ToolCommandHTTP, Effects: []string{"read", "net", "remote"}, HTTP: &fleet.ToolCommandHTTPAction{Method: "POST", URL: "https://api.example.test", Path: "/v1/predictions", TokenFile: token, Args: []fleet.ToolCommandHTTPArg{{Name: "input", Type: fleet.ToolCommandArgJSONFile, Required: true}}}}
	if res, err := Run(context.Background(), fleet.Tool{Name: "fixture"}, create, "", Options{Argv: []string{input}}); err != nil || res.Outcome != OutcomeSuccess {
		t.Fatalf("create: %+v %v", res, err)
	}
	get := fleet.ToolCommand{Name: "get", Mode: fleet.ToolCommandHTTP, Effects: []string{"read", "net", "remote"}, HTTP: &fleet.ToolCommandHTTPAction{Method: "GET", URL: "https://api.example.test", Path: "/v1/predictions/{id}", TokenFile: token, Args: []fleet.ToolCommandHTTPArg{{Name: "id", Required: true}}}}
	if res, err := Run(context.Background(), fleet.Tool{Name: "fixture"}, get, "", Options{Argv: []string{"id/one"}}); err != nil || res.Outcome != OutcomeSuccess {
		t.Fatalf("get: %+v %v", res, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}
