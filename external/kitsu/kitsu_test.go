package kitsu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	projectID = "11111111-1111-4111-8111-111111111111"
	taskID    = "22222222-2222-4222-8222-222222222222"
	commentID = "33333333-3333-4333-8333-333333333333"
)

func TestConfig(t *testing.T) {
	for _, cfg := range []Config{
		{BaseURL: "http://example.com", Token: "secret"},
		{BaseURL: "https://example.com", Token: ""},
		{BaseURL: "https://u:p@example.com", Token: "secret"},
		{BaseURL: "https://example.com/elsewhere", Token: "secret"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("accepted %#v", cfg)
		}
	}
	for _, base := range []string{"https://example.com", "https://example.com/api", "http://localhost:5000", "http://127.0.0.1:5000", "http://[::1]:5000"} {
		if _, err := NewClient(Config{BaseURL: base, Token: "secret"}); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
	}
}

func TestVerifiedRoutesAndQuery(t *testing.T) {
	cases := []struct {
		op       ReadOperation
		id, path string
	}{
		{ListProjects, "", "/api/data/projects/all?name=A%26B+%2F+C"},
		{ListEpisodes, projectID, "/api/data/projects/" + projectID + "/episodes"},
		{ListSequences, projectID, "/api/data/projects/" + projectID + "/sequences"},
		{ListShots, projectID, "/api/data/projects/" + projectID + "/shots"},
		{ListTasks, projectID, "/api/data/projects/" + projectID + "/tasks"},
		{ListShotVersions, taskID, "/api/data/shots/" + taskID + "/versions"},
		{ListShotPreviews, taskID, "/api/data/shots/" + taskID + "/preview-files"},
		{ListTaskPreviews, taskID, "/api/data/tasks/" + taskID + "/previews"},
		{ListTaskComments, taskID, "/api/data/tasks/" + taskID + "/comments"},
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer canary-secret" {
			t.Error("missing scoped token")
		}
		for _, tc := range cases {
			if r.URL.RequestURI() == tc.path {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
				return
			}
		}
		t.Errorf("unexpected route %s", r.URL.RequestURI())
		http.NotFound(w, r)
	}))
	defer server.Close()
	c, err := NewClient(Config{BaseURL: server.URL + "/api", Token: "canary-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		q := Query{}
		if tc.op == ListProjects {
			q.Name = "A&B / C"
		}
		body, err := c.Read(context.Background(), tc.op, tc.id, q)
		if err != nil || string(body) != "[]" {
			t.Fatalf("%v: %s %v", tc.op, body, err)
		}
	}
	if hits.Load() != int32(len(cases)) {
		t.Fatal("missing calls")
	}
	if _, err := c.Read(context.Background(), ReadOperation(255), "", Query{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown operation: %v", err)
	}
	if _, err := c.Read(context.Background(), ListShots, "../../evil", Query{}); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("unsafe ID: %v", err)
	}
}

func TestWriteModesAnd204(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodDelete || r.URL.Path != "/api/data/tasks/"+taskID+"/comments/"+commentID {
			t.Errorf("wrong effect: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c, _ := NewClient(Config{BaseURL: server.URL, Token: "secret"})
	if _, err := c.DeleteComment(context.Background(), taskID, commentID); !errors.Is(err, ErrExecutionNotExplicit) {
		t.Fatalf("implicit write: %v", err)
	}
	dry, err := c.DeleteComment(context.Background(), taskID, commentID, WithDryRun())
	if err != nil || dry.Executed || dry.Effect.Method != "DELETE" || hits.Load() != 0 {
		t.Fatalf("dry run: %+v %v hits=%d", dry, err, hits.Load())
	}
	if _, err := c.DeleteComment(context.Background(), taskID, commentID, WithDryRun(), WithExecute()); !errors.Is(err, ErrExecutionNotExplicit) {
		t.Fatalf("ambiguous write: %v", err)
	}
	result, err := c.DeleteComment(context.Background(), taskID, commentID, WithExecute())
	if err != nil || !result.Executed || hits.Load() != 1 {
		t.Fatalf("204: %+v %v hits=%d", result, err, hits.Load())
	}
	if _, err := c.ReviewAction(context.Background(), taskID, WithExecute()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("review action: %v", err)
	}
}

func TestRedirectLimitAndSecret(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	c, _ := NewClient(Config{BaseURL: origin.URL, Token: "canary-secret", HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}})
	_, err := c.Read(context.Background(), ListProjects, "", Query{})
	if !errors.Is(err, ErrCrossOriginRedirect) || targetHits.Load() != 0 {
		t.Fatalf("redirect: %v hits=%d", err, targetHits.Load())
	}
	if strings.Contains(err.Error(), "canary-secret") {
		t.Fatal("secret leaked")
	}
}

func TestBodyAndTimeBounds(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", int(MaxBodyBytes+1))))
	}))
	defer big.Close()
	c, _ := NewClient(Config{BaseURL: big.URL, Token: "secret"})
	if _, err := c.Read(context.Background(), ListProjects, "", Query{}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("body bound: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer slow.Close()
	c, _ = NewClient(Config{BaseURL: slow.URL, Token: "secret", Timeout: 20 * time.Millisecond, HTTPClient: &http.Client{Timeout: time.Hour}})
	start := time.Now()
	if _, err := c.Read(context.Background(), ListProjects, "", Query{}); err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("timeout: %v, %v", err, time.Since(start))
	}
}
