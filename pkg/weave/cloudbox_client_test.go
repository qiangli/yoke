package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPSessionClientMethods(t *testing.T) {
	c := newTestHTTPSessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/tasks":
			if r.Method != http.MethodGet {
				t.Errorf("tasks method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"tasks":[{"id":"task-1","name":"one","display":"Task One","goal":"ship","done_criteria":"tests pass","status":"open","summary":"summary","lease_holder":"codex@host","lease_epoch":3,"created":"2026-06-23T01:02:03Z","modified":"2026-06-23T02:03:04Z"}]}`))
		case "/api/v1/tasks/task-1/events":
			if r.Method == http.MethodGet {
				if got := r.URL.Query().Get("since"); got != "cursor-1" {
					t.Errorf("since = %q", got)
				}
				if got := r.URL.Query().Get("limit"); got != "25" {
					t.Errorf("limit = %q", got)
				}
				_, _ = w.Write([]byte(`{"events":[{"id":"ev-1","kind":"note","summary":"hello","detail":{"x":1},"created":"2026-06-23T03:04:05Z","lease_epoch":3}],"cursor":"cursor-2","lease_epoch":3}`))
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("append method = %s", r.Method)
			}
			var req AppendEventReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Kind != "note" || req.Summary != "hello" || req.LeaseEpoch == nil || *req.LeaseEpoch != 3 {
				t.Errorf("append req = %+v", req)
			}
			var detail map[string]int
			if err := json.Unmarshal(req.Detail, &detail); err != nil {
				t.Fatal(err)
			}
			if detail["x"] != 1 {
				t.Errorf("detail = %s", req.Detail)
			}
			_, _ = w.Write([]byte(`{"id":"ev-2","kind":"note","summary":"hello","detail":{"x":1},"created":"2026-06-23T04:05:06Z","lease_epoch":3}`))
		case "/api/v1/sprints":
			if r.Method != http.MethodPost {
				t.Errorf("create sprint method = %s", r.Method)
			}
			var req CreateSprintReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.TargetRepo != "/repo" || req.Gate != "go test ./..." || req.TaskID != "task-1" || len(req.Fleet) != 1 || req.Fleet[0] != "codex" {
				t.Errorf("create sprint req = %+v", req)
			}
			_, _ = w.Write([]byte(`{"id":"sprint-1","target_repo":"/repo","gate":"go test ./...","task_id":"task-1","fleet":["codex"]}`))
		case "/api/v1/sprints/sprint-1/runs":
			if r.Method != http.MethodPost {
				t.Errorf("upsert run method = %s", r.Method)
			}
			var req UpsertRunReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Issue != "#1 title" || req.Agent != "codex" || req.Sandbox != "/repo/work" || req.Status != "verified" || req.CommitsAhead != 2 || req.Exit != 0 {
				t.Errorf("upsert run req = %+v", req)
			}
			_, _ = w.Write([]byte(`{"id":"run-1","sprint_id":"sprint-1","issue":"#1 title","status":"verified"}`))
		case "/api/v1/tasks/task-1/join":
			if r.Method != http.MethodPost {
				t.Errorf("join method = %s", r.Method)
			}
			var req JoinReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Participant != "tool@host" || req.Host != "host" || req.Tool != "codex" || req.Role != "contributor" {
				t.Errorf("join req = %+v", req)
			}
			_, _ = w.Write([]byte(`{"task":{"id":"task-1","goal":"ship","done_criteria":"tests pass","status":"open","summary":"summary","lease_holder":"codex@host","lease_epoch":4},"context":{"note":[{"id":"ctx-1","kind":"note","summary":"context","detail":{"y":2},"created":"2026-06-23T05:06:07Z"}]},"cursor":"cursor-3"}`))
		case "/api/v1/tasks/task-1/lease":
			if r.Method != http.MethodPost {
				t.Errorf("lease method = %s", r.Method)
			}
			var req LeaseReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Action != "claim" || req.Holder != "codex@host" || req.TTLSeconds == nil || *req.TTLSeconds != 60 {
				t.Errorf("lease req = %+v", req)
			}
			_, _ = w.Write([]byte(`{"lease_holder":"codex@host","lease_epoch":5,"lease_expires":"2026-06-23T06:07:08Z"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	ctx := context.Background()

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-1" || tasks[0].LeaseHolder == nil || *tasks[0].LeaseHolder != "codex@host" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if want := time.Date(2026, 6, 23, 1, 2, 3, 0, time.UTC); !tasks[0].Created.Equal(want) {
		t.Errorf("created = %s, want %s", tasks[0].Created, want)
	}

	events, err := c.GetEvents(ctx, "task-1", "cursor-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if events.Cursor != "cursor-2" || len(events.Events) != 1 || string(events.Events[0].Detail) != `{"x":1}` {
		t.Fatalf("events = %+v", events)
	}

	epoch := 3
	ev, err := c.AppendEvent(ctx, "task-1", AppendEventReq{
		Kind:       "note",
		Summary:    "hello",
		Detail:     json.RawMessage(`{"x":1}`),
		LeaseEpoch: &epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "ev-2" || ev.LeaseEpoch != 3 {
		t.Fatalf("event = %+v", ev)
	}

	sprint, err := c.CreateSprint(ctx, CreateSprintReq{TargetRepo: "/repo", Gate: "go test ./...", TaskID: "task-1", Fleet: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if sprint.ID != "sprint-1" || sprint.TaskID != "task-1" {
		t.Fatalf("sprint = %+v", sprint)
	}

	run, err := c.UpsertRun(ctx, "sprint-1", UpsertRunReq{Issue: "#1 title", Agent: "codex", Sandbox: "/repo/work", Status: "verified", CommitsAhead: 2})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "run-1" || run.SprintID != "sprint-1" {
		t.Fatalf("run = %+v", run)
	}

	join, err := c.Join(ctx, "task-1", JoinReq{Participant: "tool@host", Host: "host", Tool: "codex", Role: "contributor"})
	if err != nil {
		t.Fatal(err)
	}
	if join.Cursor != "cursor-3" || join.Task.LeaseEpoch != 4 || len(join.Context["note"]) != 1 {
		t.Fatalf("join = %+v", join)
	}

	ttl := 60
	lease, err := c.Lease(ctx, "task-1", LeaseReq{Action: "claim", Holder: "codex@host", TTLSeconds: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseEpoch != 5 || lease.LeaseHolder == nil || *lease.LeaseHolder != "codex@host" {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestHTTPSessionClientStaleEpoch(t *testing.T) {
	c := newTestHTTPSessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/task-1/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"stale_epoch"}}`))
	}))

	_, err := c.AppendEvent(context.Background(), "task-1", AppendEventReq{Kind: "note"})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("err = %v, want ErrStaleEpoch", err)
	}
}

func TestHTTPSessionClientLeaseHeld(t *testing.T) {
	c := newTestHTTPSessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/task-1/lease" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"lease_held"}}`))
	}))

	_, err := c.Lease(context.Background(), "task-1", LeaseReq{Action: "claim", Holder: "codex@host"})
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestHTTPSessionClient(t *testing.T, h http.Handler) *httpSessionClient {
	t.Helper()
	c := NewHTTPSessionClient("https://cloudbox.test/", "test-token")
	c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		resp := rec.Result()
		if resp.Body == nil {
			resp.Body = io.NopCloser(bytes.NewReader(nil))
		}
		return resp, nil
	})}
	return c
}
