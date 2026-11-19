package fleet

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The wish route is runtime data: get, set (key from stdin only, never
// printed), unset and test, all with the signed-in session.
func TestDesktopPolicyRoute(t *testing.T) {
	const secret = "route-key-secret"
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
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/test"):
			w.Write([]byte(`{"ok":true,"ms":12,"source":"row"}`))
		default:
			w.Write([]byte(`{"url":"https://route.example","model":"m1","token_set":true,"source":"row"}`))
		}
	}))
	defer server.Close()
	t.Setenv(SuperadminSessionEnv, "session-test")
	run := func(stdin string, args ...string) (string, error) {
		cmd := NewDesktopCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(append(append([]string{"policy", "route"}, args...), "--master", server.URL))
		err := cmd.Execute()
		return out.String(), err
	}
	cases := []struct {
		stdin string
		args  []string
	}{
		{"", []string{"get"}},
		{"  " + secret + "\n", []string{"set", "--url", "https://route.example", "--model", "m1", "--token-stdin"}},
		{"", []string{"set", "--url", "https://route.example", "--model", "m2", "--no-probe"}},
		{"", []string{"unset"}},
		{"", []string{"test", "--wish", "make it darker"}},
		{"", []string{"test"}},
	}
	for _, c := range cases {
		out, err := run(c.stdin, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !json.Valid([]byte(out)) || strings.Contains(out, secret) {
			t.Fatalf("%v: output %q", c.args, out)
		}
	}
	const p = "/api/v1/desktop-policy/route"
	want := []struct{ method, path string }{{"GET", p}, {"PUT", p}, {"PUT", p}, {"DELETE", p}, {"POST", p + "/test"}, {"POST", p + "/test"}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, w := range want {
		if calls[i].method != w.method || calls[i].path != w.path || calls[i].auth != "Bearer session-test" {
			t.Fatalf("call %d = %+v, want %s %s with the session", i, calls[i], w.method, w.path)
		}
	}
	if b := calls[1].body; b["token"] != secret || b["probe"] != true || b["url"] != "https://route.example" || b["model"] != "m1" {
		t.Fatalf("set body = %v", b)
	}
	if b := calls[2].body; b["probe"] != false || b["model"] != "m2" {
		t.Fatalf("set --no-probe body = %v", b)
	} else if _, ok := b["token"]; ok {
		t.Fatalf("token sent without --token-stdin: %v", b)
	}
	if calls[4].body["wish"] != "make it darker" || len(calls[5].body) != 0 {
		t.Fatalf("test bodies = %v, %v", calls[4].body, calls[5].body)
	}
	// An empty key on stdin is refused before any request; a key is never a flag.
	if _, err := run(" \n", "set", "--url", "https://route.example", "--model", "m1", "--token-stdin"); err == nil {
		t.Fatal("empty stdin key must be refused")
	}
	if _, err := run("", "set", "--url", "https://route.example", "--model", "m1", "--token", secret); err == nil {
		t.Fatal("route set must not accept the key as a flag")
	}
	if _, err := run("", "set", "--model", "m1"); err == nil {
		t.Fatal("--url is required")
	}
	if len(calls) != len(want) {
		t.Fatalf("a refused route set reached the server: %d calls", len(calls))
	}
}

func TestDesktopRequestAndOwnerRequests(t *testing.T) {
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
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":7,"people":2,"new":false}`))
		default:
			w.Write([]byte(`{"requests":[{"id":7,"wish":"make it purple","people":2}]}`))
		}
	}))
	defer server.Close()
	t.Setenv(SuperadminSessionEnv, "session-test")
	run := func(args ...string) (string, error) {
		cmd := NewDesktopCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--master", server.URL))
		err := cmd.Execute()
		return out.String(), err
	}
	if out, err := run("request", "make", "it", "purple", "--reason", "theme: unsupported value", "--token", "person-token"); err != nil || !strings.Contains(out, `"people":2`) {
		t.Fatalf("request: %q %v", out, err)
	}
	if out, err := run("policy", "requests", "ls"); err != nil || !strings.Contains(out, "make it purple") {
		t.Fatalf("ls: %q %v", out, err)
	}
	if _, err := run("policy", "requests", "rm", "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("policy", "requests", "rm", "seven"); err == nil {
		t.Fatal("a non-numeric id was sent")
	}
	want := []struct{ method, path, auth string }{
		{"POST", "/api/v1/desktop/request", "Bearer person-token"},
		{"GET", "/api/v1/desktop-policy/requests", "Bearer session-test"},
		{"DELETE", "/api/v1/desktop-policy/requests?id=7", "Bearer session-test"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, w := range want {
		if calls[i].method != w.method || calls[i].path != w.path || calls[i].auth != w.auth {
			t.Fatalf("call %d = %+v, want %+v", i, calls[i], w)
		}
	}
	if calls[0].body["wish"] != "make it purple" || calls[0].body["reason"] != "theme: unsupported value" {
		t.Fatalf("request body = %v", calls[0].body)
	}
}
