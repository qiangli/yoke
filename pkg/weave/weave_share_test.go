package weave

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

func TestWeaveSharePostsDefaultObserver(t *testing.T) {
	repo := setupShareSessionRepo(t)
	var called int
	useShareHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/task-1/shares" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req GrantShareReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.ShareeEmail != "teammate@example.com" || req.Role != "observer" {
			t.Fatalf("share req = %+v", req)
		}
		_, _ = w.Write([]byte(`{"id":"share-1","task_id":"task-1","sharee_email":"teammate@example.com","role":"observer","created":"2026-06-24T01:02:03Z"}`))
	}))
	writeSharePointer(t, repo)

	out, code := runSprintSession(t, "share", "teammate@example.com", "--json")
	if code != 0 {
		t.Fatalf("share exit=%d out=%s", code, out)
	}
	if called != 1 {
		t.Fatalf("http calls=%d want 1", called)
	}
	var env struct {
		Command string    `json:"command"`
		Status  string    `json:"status"`
		Result  TaskShare `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Command != "weave share" || env.Status != "ok" || env.Result.ShareeEmail != "teammate@example.com" || env.Result.Role != "observer" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestWeaveShareBadRoleDoesNotCallHTTP(t *testing.T) {
	repo := setupShareSessionRepo(t)
	var called int
	useShareHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	writeSharePointer(t, repo)

	out, code := runSprintSession(t, "share", "teammate@example.com", "--role", "admin", "--json")
	if code != weavecli.ExitInvalidArg {
		t.Fatalf("code=%d want %d out=%s", code, weavecli.ExitInvalidArg, out)
	}
	if called != 0 {
		t.Fatalf("http calls=%d want 0", called)
	}
	if !strings.Contains(out, `"code": "invalid_arg"`) {
		t.Fatalf("missing invalid_arg envelope: %s", out)
	}
}

func TestWeaveSharesGetsEnvelope(t *testing.T) {
	repo := setupShareSessionRepo(t)
	useShareHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/tasks/task-1/shares" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"shares":[{"id":"share-1","task_id":"task-1","sharee_email":"a@example.com","role":"observer","created":"2026-06-24T01:02:03Z"},{"id":"share-2","task_id":"task-1","sharee_email":"b@example.com","role":"contributor","created":"2026-06-24T02:03:04Z"}]}`))
	}))
	writeSharePointer(t, repo)

	out, code := runSprintSession(t, "shares", "--json")
	if code != 0 {
		t.Fatalf("shares exit=%d out=%s", code, out)
	}
	var env struct {
		Command string      `json:"command"`
		Status  string      `json:"status"`
		Result  []TaskShare `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Command != "weave shares" || env.Status != "ok" || len(env.Result) != 2 || env.Result[1].Role != "contributor" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestWeaveUnshareDeletesEscapedEmail(t *testing.T) {
	repo := setupShareSessionRepo(t)
	useShareHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/v1/tasks/task-1/shares/person%2Ftag@example.com" {
			t.Fatalf("unexpected request %s path=%s escaped=%s", r.Method, r.URL.Path, r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	writeSharePointer(t, repo)

	out, code := runSprintSession(t, "unshare", "person/tag@example.com", "--json")
	if code != 0 {
		t.Fatalf("unshare exit=%d out=%s", code, out)
	}
	var env struct {
		Command string `json:"command"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Command != "weave unshare" || env.Status != "ok" {
		t.Fatalf("envelope = %+v", env)
	}
}

func setupShareSessionRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})
	t.Setenv("HOME", home)
	t.Setenv("CLOUDBOX_TOKEN", "test-token")
	t.Setenv("BASHY_AGENTIC", "")
	return cwd
}

func useShareHTTPClient(t *testing.T, h http.Handler) {
	t.Helper()
	old := newSessionClient
	newSessionClient = func(base, token string) SessionClient {
		return newTestHTTPSessionClient(t, h)
	}
	t.Cleanup(func() {
		newSessionClient = old
	})
}

func writeSharePointer(t *testing.T, repo string) {
	t.Helper()
	if err := WriteSessionPointer(repo, &SessionPointer{TaskID: "task-1", CloudboxBase: "https://cloudbox.test"}); err != nil {
		t.Fatal(err)
	}
}
