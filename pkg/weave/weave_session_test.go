package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

type fakeSessionClient struct {
	tasks       []TaskSummary
	eventsPolls []EventsResponse
	joins       []JoinReq
	appends     []AppendEventReq
	appendTasks []string
	sprints     []CreateSprintReq
	runs        []UpsertRunReq
	runSprints  []string
	leases      []LeaseReq
	shares      []GrantShareReq
	revokes     []string
	leaseErr    error
}

func (f *fakeSessionClient) ListTasks(ctx context.Context) ([]TaskSummary, error) {
	return f.tasks, nil
}

func (f *fakeSessionClient) GetEvents(ctx context.Context, taskID, since string, limit int) (EventsResponse, error) {
	if len(f.eventsPolls) == 0 {
		return EventsResponse{}, nil
	}
	resp := f.eventsPolls[0]
	f.eventsPolls = f.eventsPolls[1:]
	return resp, nil
}

func (f *fakeSessionClient) AppendEvent(ctx context.Context, taskID string, req AppendEventReq) (Event, error) {
	f.appends = append(f.appends, req)
	f.appendTasks = append(f.appendTasks, taskID)
	return Event{ID: "ev-" + req.Kind, Kind: req.Kind, Summary: req.Summary, Detail: req.Detail}, nil
}

func (f *fakeSessionClient) CreateSprint(ctx context.Context, req CreateSprintReq) (SprintSummary, error) {
	f.sprints = append(f.sprints, req)
	return SprintSummary{ID: "sprint-1", TargetRepo: req.TargetRepo, Gate: req.Gate, TaskID: req.TaskID}, nil
}

func (f *fakeSessionClient) UpsertRun(ctx context.Context, sprintID string, req UpsertRunReq) (RunSummary, error) {
	f.runs = append(f.runs, req)
	f.runSprints = append(f.runSprints, sprintID)
	return RunSummary{ID: "run-1", SprintID: sprintID, Issue: req.Issue, Status: req.Status}, nil
}

func (f *fakeSessionClient) Join(ctx context.Context, taskID string, req JoinReq) (JoinResponse, error) {
	f.joins = append(f.joins, req)
	return JoinResponse{Task: JoinTask{ID: taskID, Goal: "goal", Status: "active"}, Cursor: "c0"}, nil
}

func (f *fakeSessionClient) Lease(ctx context.Context, taskID string, req LeaseReq) (LeaseResponse, error) {
	f.leases = append(f.leases, req)
	if f.leaseErr != nil {
		return LeaseResponse{}, f.leaseErr
	}
	return LeaseResponse{LeaseHolder: &req.Holder, LeaseEpoch: len(f.leases)}, nil
}

func (f *fakeSessionClient) GrantShare(ctx context.Context, taskID string, req GrantShareReq) (TaskShare, error) {
	f.shares = append(f.shares, req)
	return TaskShare{ID: "share-1", TaskID: taskID, ShareeEmail: req.ShareeEmail, Role: req.Role}, nil
}

func (f *fakeSessionClient) ListShares(ctx context.Context, taskID string) ([]TaskShare, error) {
	return []TaskShare{{ID: "share-1", TaskID: taskID, ShareeEmail: "a@example.com", Role: "observer"}}, nil
}

func (f *fakeSessionClient) RevokeShare(ctx context.Context, taskID, sharee string) error {
	f.revokes = append(f.revokes, sharee)
	return nil
}

func TestResolveJoinTaskID(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSessionClient{tasks: []TaskSummary{
		{ID: "a", Status: "done"},
		{ID: "b", Status: "active"},
	}}
	if got, err := resolveJoinTaskID(ctx, fake, "explicit", nil); err != nil || got != "explicit" {
		t.Fatalf("explicit got %q err %v", got, err)
	}
	if got, err := resolveJoinTaskID(ctx, fake, "", &SessionPointer{TaskID: "ptr"}); err != nil || got != "ptr" {
		t.Fatalf("pointer got %q err %v", got, err)
	}
	if got, err := resolveJoinTaskID(ctx, fake, "", &SessionPointer{}); err != nil || got != "b" {
		t.Fatalf("single active got %q err %v", got, err)
	}
	fake.tasks = append(fake.tasks, TaskSummary{ID: "c", Status: "working"})
	if _, err := resolveJoinTaskID(ctx, fake, "", &SessionPointer{}); err == nil || !strings.Contains(err.Error(), "multiple active") {
		t.Fatalf("ambiguous err = %v", err)
	}
}

func TestSessionFeedLoopAdvancesCursorAcrossPolls(t *testing.T) {
	fake := &fakeSessionClient{eventsPolls: []EventsResponse{
		{Events: []Event{{ID: "e1", Kind: "note", Summary: "one"}}, Cursor: "c1"},
		{Events: []Event{{ID: "e2", Kind: "directive", Summary: "two"}}, Cursor: "c2"},
	}}
	var out bytes.Buffer
	sleeps := 0
	cursor, err := sessionFeedLoop(context.Background(), &out, fake, "task", "c0", 2, func(context.Context) error {
		sleeps++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c2" {
		t.Fatalf("cursor=%q want c2", cursor)
	}
	if sleeps != 1 {
		t.Fatalf("sleeps=%d want 1", sleeps)
	}
	if got := out.String(); !strings.Contains(got, "[note] one") || !strings.Contains(got, "[directive] two") {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestSessionActionsProduceClientCalls(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSessionClient{}
	if _, err := appendSessionEvent(ctx, fake, "task", AppendEventReq{Kind: "note", Summary: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.appends) != 1 || fake.appends[0].Kind != "note" || fake.appends[0].Summary != "hello" {
		t.Fatalf("note append = %+v", fake.appends)
	}

	detail, _ := json.Marshal(map[string]string{"run": "r1", "verb": "say", "arg": "go"})
	if _, err := appendSessionEvent(ctx, fake, "task", AppendEventReq{Kind: "directive", Summary: "go", Detail: detail}); err != nil {
		t.Fatal(err)
	}
	var steer struct {
		Run  string `json:"run"`
		Verb string `json:"verb"`
		Arg  string `json:"arg"`
	}
	if err := json.Unmarshal(fake.appends[1].Detail, &steer); err != nil {
		t.Fatal(err)
	}
	if steer.Run != "r1" || steer.Verb != "say" || steer.Arg != "go" {
		t.Fatalf("steer detail = %+v", steer)
	}

	if _, err := takeSessionLease(ctx, fake, "task", "me@host", true, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(fake.leases) != 1 || fake.leases[0].Action != "force" || fake.leases[0].Holder != "me@host" || fake.leases[0].TTLSeconds == nil || *fake.leases[0].TTLSeconds != 5 {
		t.Fatalf("take lease = %+v", fake.leases)
	}

	if _, _, err := handoffSession(ctx, fake, "task", "you@host"); err != nil {
		t.Fatal(err)
	}
	if fake.appends[2].Kind != "handoff" {
		t.Fatalf("handoff append = %+v", fake.appends[2])
	}
	var handoff struct {
		To   string `json:"to"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(fake.appends[2].Detail, &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.To != "you@host" || handoff.Mode != "manual" {
		t.Fatalf("handoff detail = %+v", handoff)
	}
	if fake.leases[1].Action != "release" {
		t.Fatalf("handoff lease = %+v", fake.leases[1])
	}
}

func TestSessionArgValidationExitCodes(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "join too many", args: []string{"join", "a", "b", "--json"}},
		{name: "note missing", args: []string{"note", "--json"}},
		{name: "steer missing", args: []string{"steer", "run", "--json"}},
		{name: "handoff missing to", args: []string{"handoff", "--json"}},
		{name: "list extra", args: []string{"list", "x", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runSprintSession(t, tc.args...)
			if code != weavecli.ExitInvalidArg {
				t.Fatalf("code=%d want %d out=%s", code, weavecli.ExitInvalidArg, out)
			}
			if !strings.Contains(out, "invalid_arg") {
				t.Fatalf("missing invalid_arg envelope: %s", out)
			}
		})
	}
}

func TestTakeLeaseHeldErrorIsRecognized(t *testing.T) {
	fake := &fakeSessionClient{leaseErr: fmtLeaseHeld("alice")}
	_, err := takeSessionLease(context.Background(), fake, "task", "me", false, 0)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err=%v", err)
	}
	if got := leaseHeldUserError(err).Error(); got != "held by alice; use --force" {
		t.Fatalf("message=%q", got)
	}
	jsonErr := errors.Join(ErrLeaseHeld, errors.New(`409 Conflict: {"error":{"code":"lease_held","lease_holder":"bob"}}`))
	if got := leaseHeldUserError(jsonErr).Error(); got != "held by bob; use --force" {
		t.Fatalf("json message=%q", got)
	}
}

func fmtLeaseHeld(holder string) error {
	return errors.Join(ErrLeaseHeld, errors.New("held by "+holder))
}
