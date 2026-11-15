package fleet

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDesktopParity(t *testing.T) {
	var writes []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer desktop-test" {
			t.Error("lost existing credential")
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"revision":7,"resolved":{"shapeOnly":false}}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		writes = append(writes, body)
		w.Write([]byte(`{"revision":8}`))
	}))
	defer server.Close()
	for _, args := range [][]string{{"schema"}, {"get"}, {"catalog"}, {"history"}, {"allowance"}, {"set", "theme", "light", "--dry-run"}, {"shape-only"}, {"wish", "make it darker"}, {"undo"}} {
		cmd := NewDesktopCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append(args, "--master", server.URL, "--token", "desktop-test", "--json"))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("invalid JSON: %s", out.Bytes())
		}
	}
	if len(writes) != 4 {
		t.Fatal(writes)
	}
	if writes[0]["dry_run"] != true || writes[0]["expected_revision"] != float64(7) {
		t.Fatal(writes[0])
	}
	if writes[1]["patch"].(map[string]any)["shapeOnly"] != true {
		t.Fatal(writes[1])
	}
	if writes[2]["wish"] != "make it darker" {
		t.Fatal(writes[2])
	}
}
func TestDesktopConflictNeverRetries(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(409)
		w.Write([]byte(`{"code":"revision_conflict","error":"stale"}`))
	}))
	defer server.Close()
	cmd := NewDesktopCmd()
	cmd.SetArgs([]string{"set", "theme", "light", "--revision", "3", "--master", server.URL, "--token", "test"})
	err := cmd.Execute()
	if DesktopExitCode(err) != 3 || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestDesktopInvokeAwaitsResult(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/desktop/invoke":
			writes++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["page_id"] != "page-1" || body["action"] != "desktop.search" || body["input"] != "john" {
				t.Error(body)
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"command-1","status":"queued"}`))
		case "/api/v1/desktop/result":
			if r.URL.Query().Get("id") != "command-1" {
				t.Error(r.URL)
			}
			w.Write([]byte(`{"id":"command-1","status":"completed","result":{"saved":true}}`))
		default:
			t.Error(r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	for _, args := range [][]string{{"invoke", "page-1", "desktop.search", `"john"`}, {"result", "command-1"}} {
		cmd := NewDesktopCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append(args, "--master", server.URL, "--token", "test"))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Status != "completed" {
			t.Fatal(out.String(), err)
		}
	}
	if writes != 1 {
		t.Fatal("replayed command", writes)
	}
}

// The allowance policy is set with a signed-in session, never an API token,
// on the same paths the operator console uses.
func TestDesktopPolicyUsesSessionAndRoute(t *testing.T) {
	type call struct {
		method, path, auth string
		body               map[string]any
	}
	var calls []call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{method: r.Method, path: r.URL.RequestURI(), auth: r.Header.Get("Authorization")}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&c.body)
		}
		calls = append(calls, c)
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		w.Write([]byte(`{"owner":"*","daily_limit":5}`))
	}))
	defer server.Close()
	t.Setenv(SuperadminSessionEnv, "session-test")
	for _, args := range [][]string{{"policy", "ls"}, {"policy", "set", "*", "5"}, {"policy", "unset", "ann@example.com"}} {
		cmd := NewDesktopCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append(args, "--master", server.URL))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("%v: invalid JSON %q", args, out.Bytes())
		}
	}
	want := []struct{ method, path string }{
		{"GET", "/api/v1/desktop-policy/allowance"},
		{"PUT", "/api/v1/desktop-policy/allowance"},
		{"DELETE", "/api/v1/desktop-policy/allowance?owner=ann%40example.com"},
	}
	for i, w := range want {
		if calls[i].method != w.method || calls[i].path != w.path || calls[i].auth != "Bearer session-test" {
			t.Fatalf("call %d = %+v, want %s %s with the session", i, calls[i], w.method, w.path)
		}
	}
	if calls[1].body["owner"] != "*" || calls[1].body["daily_limit"] != float64(5) {
		t.Fatalf("set body = %v", calls[1].body)
	}
	t.Setenv(SuperadminSessionEnv, "")
	cmd := NewDesktopCmd()
	cmd.SetArgs([]string{"policy", "ls", "--master", server.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatal("policy without a session must be refused before any request")
	}
	if len(calls) != 3 {
		t.Fatalf("a refused policy call still reached the server: %d calls", len(calls))
	}
}

// Unavailable wishes are a refusal with a reason: exit 3 and a JSON
// envelope carrying the status and the server's reason code.
func TestDesktopUnavailableIsARefusalWithReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"revision":1}`))
			return
		}
		w.WriteHeader(503)
		w.Write([]byte(`{"code":"pool_unreachable","error":"the model pool cannot be reached right now; ordinary settings still work"}`))
	}))
	defer server.Close()
	cmd := NewDesktopCmd()
	cmd.SetArgs([]string{"wish", "make it dark", "--master", server.URL, "--token", "t"})
	err := cmd.Execute()
	if DesktopExitCode(err) != 3 {
		t.Fatalf("exit %d for %v; want 3", DesktopExitCode(err), err)
	}
	var env map[string]any
	if json.Unmarshal([]byte(DesktopErrorJSON(err)), &env) != nil || env["status"] != float64(503) || env["exit"] != float64(3) {
		t.Fatalf("envelope %s", DesktopErrorJSON(err))
	}
}
