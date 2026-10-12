package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSyncRead_PostBodyAndBearer(t *testing.T) {
	var receivedAuth string
	var receivedContentType string
	var receivedBody SyncReadRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/feed/read" {
			t.Errorf("expected /api/v1/feed/read, got %s", r.URL.Path)
		}
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"marked":2}`))
	}))
	defer srv.Close()

	env := Env{
		Cloud: func() (string, string) {
			return srv.URL, "secret-token"
		},
	}

	err := SyncReadEnv(context.Background(), env, []string{"post-1", "post-2"})
	if err != nil {
		t.Fatalf("SyncReadEnv failed: %v", err)
	}

	if receivedAuth != "Bearer secret-token" {
		t.Errorf("expected Bearer secret-token, got %q", receivedAuth)
	}
	if receivedContentType != "application/json" {
		t.Errorf("expected application/json, got %q", receivedContentType)
	}
	if len(receivedBody.IDs) != 2 || receivedBody.IDs[0] != "post-1" || receivedBody.IDs[1] != "post-2" {
		t.Errorf("unexpected body ids: %v", receivedBody.IDs)
	}
}

func TestSyncRead_UnpairedIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected HTTP call when unpaired")
	}))
	defer srv.Close()

	// Empty token means unpaired
	env := Env{
		Cloud: func() (string, string) {
			return srv.URL, ""
		},
	}

	err := SyncReadEnv(context.Background(), env, []string{"post-1"})
	if err != nil {
		t.Fatalf("expected nil error when unpaired, got: %v", err)
	}

	// Also test global SyncRead with empty IDs
	if err := SyncRead(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error for empty IDs, got: %v", err)
	}
}

func TestFetchServerUnread_And_ServerReadSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/feed" {
			t.Errorf("expected /api/v1/feed, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("unread") != "1" {
			t.Errorf("expected unread=1, got %q", r.URL.Query().Get("unread"))
		}
		if r.Header.Get("Authorization") != "Bearer token-feed-read" {
			t.Errorf("expected Bearer token-feed-read, got %q", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"timeline": "home",
			"posts": [
				{"id": "post-unread-1"},
				{"id": "post-unread-2"}
			],
			"next": ""
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, "token-feed-read"
		},
	}

	// 1. Test unread parsing via FetchServerUnread
	unread, err := FetchServerUnread(context.Background(), env)
	if err != nil {
		t.Fatalf("FetchServerUnread failed: %v", err)
	}
	if !unread["post-unread-1"] || !unread["post-unread-2"] {
		t.Errorf("expected post-unread-1 and post-unread-2 in unread set: %v", unread)
	}
	if unread["post-read-3"] {
		t.Errorf("post-read-3 should not be in unread set")
	}

	// 2. Populate spool with post-unread-1, post-unread-2, and post-read-3
	now := time.Now()
	for _, p := range []*Post{
		{ID: "post-unread-1", Headline: "P1", Created: now.Format(time.RFC3339)},
		{ID: "post-unread-2", Headline: "P2", Created: now.Format(time.RFC3339)},
		{ID: "post-read-3", Headline: "P3", Created: now.Format(time.RFC3339)},
	} {
		raw, err := p.Marshal()
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}
		if _, err := Ingest(dir, "src", "posts/p.md", raw, now); err != nil {
			t.Fatalf("Ingest failed: %v", err)
		}
	}

	// 3. Test ServerReadSetEnv: known posts NOT in unread list should be in readSet
	readSet, err := ServerReadSetEnv(context.Background(), env)
	if err != nil {
		t.Fatalf("ServerReadSetEnv failed: %v", err)
	}
	if !readSet["post-read-3"] {
		t.Errorf("expected post-read-3 to be marked read in ServerReadSet")
	}
	if readSet["post-unread-1"] {
		t.Errorf("post-unread-1 should not be marked read in ServerReadSet")
	}
	if readSet["post-unread-2"] {
		t.Errorf("post-unread-2 should not be marked read in ServerReadSet")
	}

	// 4. Test ServerReadFunc predicate
	pred, err := ServerReadFunc(context.Background(), env)
	if err != nil {
		t.Fatalf("ServerReadFunc failed: %v", err)
	}
	if !pred("post-read-3") {
		t.Errorf("pred(post-read-3) should be true (read)")
	}
	if pred("post-unread-1") {
		t.Errorf("pred(post-unread-1) should be false (unread)")
	}

	// 5. Test NewServerReadFunc constructor
	constructor := NewServerReadFunc(env)
	cReadSet, err := constructor(context.Background())
	if err != nil {
		t.Fatalf("NewServerReadFunc constructor failed: %v", err)
	}
	if !cReadSet["post-read-3"] {
		t.Errorf("expected post-read-3 in constructor readSet")
	}
}

func TestFeedReadCmd_SyncHookSuccess(t *testing.T) {
	var mu sync.Mutex
	var synced []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SyncReadRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)

		mu.Lock()
		synced = append(synced, req.IDs...)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"marked": 1}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	now := time.Now()
	for _, id := range []string{"post-a", "post-b"} {
		p := &Post{ID: id, Headline: id, Created: now.Format(time.RFC3339)}
		raw, err := p.Marshal()
		if err != nil {
			t.Fatalf("Marshal %s failed: %v", id, err)
		}
		if _, err := Ingest(dir, "src", filepath.Join("posts", id+".md"), raw, now); err != nil {
			t.Fatalf("Ingest %s failed: %v", id, err)
		}
	}

	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, "auth-token"
		},
	}

	cmd := NewCmdWith(env)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"read", "post-a"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("read post-a failed: %v", err)
	}
	if !strings.Contains(out.String(), "cursor 1") {
		t.Errorf("expected cursor 1 in stdout, got %q", out.String())
	}

	mu.Lock()
	if len(synced) != 1 || synced[0] != "post-a" {
		t.Errorf("expected synced [post-a], got %v", synced)
	}
	mu.Unlock()

	// Now feed read --all
	out.Reset()
	errBuf.Reset()
	cmd = NewCmdWith(env)
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"read", "--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("read --all failed: %v", err)
	}
	if !strings.Contains(out.String(), "cursor 2") {
		t.Errorf("expected cursor 2 in stdout, got %q", out.String())
	}

	mu.Lock()
	if len(synced) != 2 || synced[1] != "post-b" {
		t.Errorf("expected synced [post-a, post-b], got %v", synced)
	}
	mu.Unlock()
}

func TestFeedReadCmd_Server500ProducesWarningOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	now := time.Now()
	p := &Post{ID: "post-err", Headline: "Error test", Created: now.Format(time.RFC3339)}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if _, err := Ingest(dir, "src", "posts/err.md", raw, now); err != nil {
		t.Fatalf("Ingest failed: %v", err)
	}

	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, "auth-token"
		},
	}

	cmd := NewCmdWith(env)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"read", "post-err"})

	// Command MUST exit 0 because local read state is authoritative offline.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("read command should succeed even if sync fails, got err: %v", err)
	}

	if !strings.Contains(out.String(), "cursor 1") {
		t.Errorf("expected cursor 1 in stdout, got %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "feed: read sync failed:") {
		t.Errorf("expected warning in stderr, got %q", errBuf.String())
	}
}

func TestFeedReadCmd_UnpairedIsSilent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := &Post{ID: "post-unpaired", Headline: "Unpaired test", Created: now.Format(time.RFC3339)}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if _, err := Ingest(dir, "src", "posts/unpaired.md", raw, now); err != nil {
		t.Fatalf("Ingest failed: %v", err)
	}

	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return "http://nowhere.invalid", "" // unpaired
		},
	}

	cmd := NewCmdWith(env)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"read", "post-unpaired"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("read command failed: %v", err)
	}

	if !strings.Contains(out.String(), "cursor 1") {
		t.Errorf("expected cursor 1 in stdout, got %q", out.String())
	}
	if errBuf.String() != "" {
		t.Errorf("expected no stderr when unpaired, got %q", errBuf.String())
	}
}
