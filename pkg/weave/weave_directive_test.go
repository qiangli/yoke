package weave

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type directiveMockSessionClient struct {
	eventsByCursor map[string]EventsResponse
	appended       []AppendEventReq
}

func (m *directiveMockSessionClient) ListTasks(ctx context.Context) ([]TaskSummary, error) {
	return nil, nil
}

func (m *directiveMockSessionClient) GetEvents(ctx context.Context, taskID, since string, limit int) (EventsResponse, error) {
	return m.eventsByCursor[since], nil
}

func (m *directiveMockSessionClient) AppendEvent(ctx context.Context, taskID string, req AppendEventReq) (Event, error) {
	m.appended = append(m.appended, req)
	return Event{ID: "ack"}, nil
}

func (m *directiveMockSessionClient) CreateSprint(ctx context.Context, req CreateSprintReq) (SprintSummary, error) {
	return SprintSummary{}, nil
}

func (m *directiveMockSessionClient) UpsertRun(ctx context.Context, sprintID string, req UpsertRunReq) (RunSummary, error) {
	return RunSummary{}, nil
}

func (m *directiveMockSessionClient) Join(ctx context.Context, taskID string, req JoinReq) (JoinResponse, error) {
	return JoinResponse{}, nil
}

func (m *directiveMockSessionClient) Lease(ctx context.Context, taskID string, req LeaseReq) (LeaseResponse, error) {
	return LeaseResponse{}, nil
}

func (m *directiveMockSessionClient) GrantShare(ctx context.Context, taskID string, req GrantShareReq) (TaskShare, error) {
	return TaskShare{}, nil
}

func (m *directiveMockSessionClient) ListShares(ctx context.Context, taskID string) ([]TaskShare, error) {
	return nil, nil
}

func (m *directiveMockSessionClient) RevokeShare(ctx context.Context, taskID, sharee string) error {
	return nil
}

func TestConsumeDirectivesAppliesAndAcks(t *testing.T) {
	tests := []struct {
		name   string
		verb   string
		arg    string
		assert func(t *testing.T, queueDir, ctlPath string)
	}{
		{
			name: "say",
			verb: "say",
			arg:  "focus on the failing test",
			assert: func(t *testing.T, queueDir, ctlPath string) {
				b, err := os.ReadFile(ctlPath)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := string(b), "focus on the failing test\r\n"; got != want {
					t.Fatalf("control payload = %q, want %q", got, want)
				}
			},
		},
		{
			name: "add",
			verb: "add",
			arg:  "new remote issue",
			assert: func(t *testing.T, queueDir, ctlPath string) {
				q, err := loadWeaveQueue(queueDir)
				if err != nil {
					t.Fatal(err)
				}
				if got := len(q.Items); got != 2 {
					t.Fatalf("queue item count = %d, want 2", got)
				}
				if got := q.Items[1].Title; got != "new remote issue" {
					t.Fatalf("added title = %q", got)
				}
				if got := q.Items[1].State; got != "todo" {
					t.Fatalf("added state = %q", got)
				}
			},
		},
		{
			name: "prio",
			verb: "prio",
			arg:  "p0",
			assert: func(t *testing.T, queueDir, ctlPath string) {
				q, err := loadWeaveQueue(queueDir)
				if err != nil {
					t.Fatal(err)
				}
				if got := q.Items[0].Priority; got != "p0" {
					t.Fatalf("priority = %q, want p0", got)
				}
			},
		},
		{
			name: "kill",
			verb: "kill",
			arg:  "remote stop",
			assert: func(t *testing.T, queueDir, ctlPath string) {
				q, err := loadWeaveQueue(queueDir)
				if err != nil {
					t.Fatal(err)
				}
				if got := q.Items[0].State; got != "killed" {
					t.Fatalf("state = %q, want killed", got)
				}
				if q.Items[0].ExitCode == nil || *q.Items[0].ExitCode != -1 {
					t.Fatalf("exit code = %v, want -1", q.Items[0].ExitCode)
				}
			},
		},
		{
			name: "prioritize",
			verb: "prioritize",
			arg:  "p1",
			assert: func(t *testing.T, queueDir, ctlPath string) {
				q, err := loadWeaveQueue(queueDir)
				if err != nil {
					t.Fatal(err)
				}
				if got := q.Items[0].Priority; got != "p1" {
					t.Fatalf("priority = %q, want p1", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queueDir, ctlPath := seedDirectiveQueue(t, tt.name == "say")
			client := &directiveMockSessionClient{eventsByCursor: map[string]EventsResponse{
				"": {
					Events: []Event{directiveEvent(t, "d1", 1, tt.verb, tt.arg)},
					Cursor: "c1",
				},
			}}

			cursor, applied, err := consumeDirectives(context.Background(), client, "task-1", queueDir, "")
			if err != nil {
				t.Fatal(err)
			}
			if cursor != "c1" {
				t.Fatalf("cursor = %q, want c1", cursor)
			}
			if applied != 1 {
				t.Fatalf("applied = %d, want 1", applied)
			}
			assertSingleAckStatus(t, client.appended, "applied")
			tt.assert(t, queueDir, ctlPath)
		})
	}
}

func TestConsumeDirectivesUnknownVerbAcksAndContinues(t *testing.T) {
	queueDir, _ := seedDirectiveQueue(t, false)
	client := &directiveMockSessionClient{eventsByCursor: map[string]EventsResponse{
		"": {
			Events: []Event{directiveEvent(t, "d1", 1, "dance", "now")},
			Cursor: "c1",
		},
	}}
	cursor, applied, err := consumeDirectives(context.Background(), client, "task-1", queueDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c1" {
		t.Fatalf("cursor = %q, want c1", cursor)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
	assertSingleAckStatus(t, client.appended, "unknown_verb")
}

func TestConsumeDirectivesSkipsNoteAndAdvancesCursor(t *testing.T) {
	queueDir, _ := seedDirectiveQueue(t, false)
	client := &directiveMockSessionClient{eventsByCursor: map[string]EventsResponse{
		"": {
			Events: []Event{{ID: "n1", Kind: "note", Summary: "FYI"}},
			Cursor: "c-note",
		},
	}}
	cursor, applied, err := consumeDirectives(context.Background(), client, "task-1", queueDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c-note" {
		t.Fatalf("cursor = %q, want c-note", cursor)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
	if got := len(client.appended); got != 0 {
		t.Fatalf("ack count = %d, want 0", got)
	}
}

func TestConsumeDirectivesCursorIdempotency(t *testing.T) {
	queueDir, _ := seedDirectiveQueue(t, false)
	client := &directiveMockSessionClient{eventsByCursor: map[string]EventsResponse{
		"": {
			Events: []Event{directiveEvent(t, "d1", 1, "prio", "p0")},
			Cursor: "c1",
		},
		"c1": {
			Events: nil,
			Cursor: "c1",
		},
	}}

	cursor, applied, err := consumeDirectives(context.Background(), client, "task-1", queueDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDirectiveCursor(queueDir, cursor); err != nil {
		t.Fatal(err)
	}
	persisted, err := readDirectiveCursor(queueDir)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("first applied = %d, want 1", applied)
	}
	cursor, applied, err = consumeDirectives(context.Background(), client, "task-1", queueDir, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c1" {
		t.Fatalf("second cursor = %q, want c1", cursor)
	}
	if applied != 0 {
		t.Fatalf("second applied = %d, want 0", applied)
	}
	if got := len(client.appended); got != 1 {
		t.Fatalf("ack count = %d, want 1", got)
	}
	q, err := loadWeaveQueue(queueDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Items[0].Priority; got != "p0" {
		t.Fatalf("priority = %q, want p0", got)
	}
}

func seedDirectiveQueue(t *testing.T, live bool) (string, string) {
	t.Helper()
	queueDir := t.TempDir()
	ctlPath := filepath.Join(queueDir, "ctl")
	if err := os.WriteFile(ctlPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wrapperPid := 0
	if live {
		wrapperPid = os.Getpid()
	}
	q := &weaveQueue{
		NextID: 2,
		Items: []*weaveItem{{
			ID:         1,
			Title:      "existing issue",
			Priority:   "p2",
			State:      "working",
			Created:    time.Now().UTC(),
			WrapperPid: wrapperPid,
			CtlSock:    ctlPath,
		}},
	}
	if err := saveWeaveQueue(queueDir, q); err != nil {
		t.Fatal(err)
	}
	return queueDir, ctlPath
}

func directiveEvent(t *testing.T, id string, run int64, verb, arg string) Event {
	t.Helper()
	detail, err := json.Marshal(map[string]any{
		"run":  run,
		"verb": verb,
		"arg":  arg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Event{ID: id, Kind: "directive", Detail: detail}
}

func assertSingleAckStatus(t *testing.T, appended []AppendEventReq, want string) {
	t.Helper()
	if got := len(appended); got != 1 {
		t.Fatalf("ack count = %d, want 1", got)
	}
	if appended[0].Kind != "ack" {
		t.Fatalf("ack kind = %q, want ack", appended[0].Kind)
	}
	var detail directiveAckDetail
	if err := json.Unmarshal(appended[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Status != want {
		t.Fatalf("ack status = %q, want %q", detail.Status, want)
	}
}
