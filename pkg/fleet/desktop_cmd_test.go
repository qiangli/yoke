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
