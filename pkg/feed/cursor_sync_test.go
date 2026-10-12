package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestServerReadSet_ExactSetAndBearer(t *testing.T) {
	const inbox = "https://inbox.example/u/7"
	var gotIDs []string
	var gotAuth string
	var calls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/feed/read" {
			t.Errorf("expected /api/v1/feed/read, got %s", r.URL.Path)
		}
		gotIDs = r.URL.Query()["ids"]
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ServerReadResponse{
			Read:  []string{"post-read-3"},
			Inbox: inbox,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, "token-feed-read"
		},
	}
	dlvSpool(t, dir,
		dlvPost{"post-unread-1", "alice", "P1"},
		dlvPost{"post-unread-2", "bob", "P2"},
		dlvPost{"post-read-3", "carol", "P3"})

	// 1. FetchServerRead sends the spool's candidate ids and the bearer.
	read, gotInbox, err := FetchServerRead(context.Background(), env,
		[]string{"post-unread-1", "post-unread-2", "post-read-3"})
	if err != nil {
		t.Fatalf("FetchServerRead failed: %v", err)
	}
	if gotAuth != "Bearer token-feed-read" {
		t.Errorf("expected Bearer token-feed-read, got %q", gotAuth)
	}
	wantIDs := map[string]bool{"post-unread-1": true, "post-unread-2": true, "post-read-3": true}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("expected ids %v, got %v", wantIDs, gotIDs)
	}
	for _, id := range gotIDs {
		if !wantIDs[id] {
			t.Fatalf("unexpected id %q in %v", id, gotIDs)
		}
	}
	if gotInbox != inbox {
		t.Errorf("expected inbox %q, got %q", inbox, gotInbox)
	}
	if len(read) != 1 || !read["post-read-3"] {
		t.Errorf("expected exactly {post-read-3}, got %v", read)
	}

	// 2. ServerReadSetEnv returns exactly the server's "read" set: older
	// unread posts and locally-followed posts are never marked read.
	readSet, err := ServerReadSetEnv(context.Background(), env)
	if err != nil {
		t.Fatalf("ServerReadSetEnv failed: %v", err)
	}
	if len(readSet) != 1 || !readSet["post-read-3"] {
		t.Errorf("expected exactly {post-read-3}, got %v", readSet)
	}

	// 3. ServerReadAndInboxEnv carries the inbox alongside the read set.
	r2, in2, err := ServerReadAndInboxEnv(context.Background(), env)
	if err != nil {
		t.Fatalf("ServerReadAndInboxEnv failed: %v", err)
	}
	if len(r2) != 1 || !r2["post-read-3"] {
		t.Errorf("expected exactly {post-read-3}, got %v", r2)
	}
	if in2 != inbox {
		t.Errorf("expected inbox %q, got %q", inbox, in2)
	}

	// 4. ServerReadFunc predicate over the same set.
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
	if pred("never-spooled") {
		t.Errorf("pred(never-spooled) should be false (unknown, not read)")
	}

	// 5. NewServerReadFunc constructor.
	constructor := NewServerReadFunc(env)
	cReadSet, err := constructor(context.Background())
	if err != nil {
		t.Fatalf("NewServerReadFunc constructor failed: %v", err)
	}
	if len(cReadSet) != 1 || !cReadSet["post-read-3"] {
		t.Errorf("expected exactly {post-read-3}, got %v", cReadSet)
	}

	// 6. NewServerReadAndInbox shares one round trip per chunk.
	dfltRead, dfltInbox := NewServerReadAndInbox(env)
	before := calls
	shared, err := dfltRead(context.Background())
	if err != nil {
		t.Fatalf("shared serverRead failed: %v", err)
	}
	if !shared["post-read-3"] {
		t.Errorf("expected post-read-3 in shared readSet, got %v", shared)
	}
	if got := dfltInbox(context.Background()); got != inbox {
		t.Errorf("expected shared inbox %q, got %q", inbox, got)
	}
	if calls-before != 1 {
		t.Errorf("expected one shared round trip, got %d calls", calls-before)
	}
}

func TestServerReadSet_ChunksAt500(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query()["ids"]
		if r.Header.Get("Authorization") != "Bearer chunk-token" {
			t.Errorf("expected Bearer chunk-token, got %q", r.Header.Get("Authorization"))
		}
		if len(ids) > 500 {
			t.Errorf("chunk of %d ids exceeds the 500 limit", len(ids))
		}
		mu.Lock()
		calls++
		seen = append(seen, ids...)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ServerReadResponse{Read: ids, Inbox: "https://inbox.example/x"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, "chunk-token"
		},
	}
	var posts []dlvPost
	for i := 0; i < 1200; i++ {
		posts = append(posts, dlvPost{fmt.Sprintf("p-%04d", i), "alice", "H"})
	}
	dlvSpool(t, dir, posts...)

	read, inbox, err := ServerReadAndInboxEnv(context.Background(), env)
	if err != nil {
		t.Fatalf("ServerReadAndInboxEnv failed: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 chunked calls for 1200 ids, got %d", calls)
	}
	if len(seen) != 1200 {
		t.Errorf("expected 1200 ids queried, got %d", len(seen))
	}
	if len(read) != 1200 {
		t.Errorf("expected 1200 read ids, got %d", len(read))
	}
	if inbox != "https://inbox.example/x" {
		t.Errorf("expected inbox URL, got %q", inbox)
	}
}

func TestServerReadSet_UnpairedIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected HTTP call when unpaired")
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Empty token means unpaired.
	env := Env{
		Dir: func() string { return dir },
		Cloud: func() (string, string) {
			return srv.URL, ""
		},
	}
	dlvSpool(t, dir, dlvPost{"post-1", "alice", "P1"})

	read, inbox, err := FetchServerRead(context.Background(), env, []string{"post-1"})
	if err != nil {
		t.Fatalf("expected nil error when unpaired, got: %v", err)
	}
	if read != nil {
		t.Errorf("expected nil map when unpaired, got %v", read)
	}
	if inbox != "" {
		t.Errorf("expected empty inbox when unpaired, got %q", inbox)
	}

	set, err := ServerReadSetEnv(context.Background(), env)
	if err != nil {
		t.Fatalf("expected nil error when unpaired, got: %v", err)
	}
	if set != nil {
		t.Errorf("expected nil map when unpaired, got %v", set)
	}

	dfltRead, dfltInbox := NewServerReadAndInbox(env)
	if r, err := dfltRead(context.Background()); err != nil || r != nil {
		t.Errorf("expected nil map and nil error when unpaired, got %v %v", r, err)
	}
	if got := dfltInbox(context.Background()); got != "" {
		t.Errorf("expected empty inbox when unpaired, got %q", got)
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
