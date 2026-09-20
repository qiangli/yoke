package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrStaleEpoch = errors.New("stale lease epoch")
	ErrLeaseHeld  = errors.New("lease held")
)

type SessionClient interface {
	ListTasks(ctx context.Context) ([]TaskSummary, error)
	CreateTask(ctx context.Context, req CreateTaskReq) (TaskSummary, error)
	// ListTasksByRepo asks `GET /api/v1/tasks?repo=<key>`. A cloudbox that
	// knows the query answers with sessions split into reachable and
	// joinable (ErrRepoQueryUnsupported otherwise, and the caller filters
	// ListTasks itself).
	ListTasksByRepo(ctx context.Context, repo string) (RepoSessions, error)
	// JoinByRepo is `POST /api/v1/tasks/join-by-repo`: the server seats the
	// caller on the repo's session by GitHub's answer when no share exists.
	JoinByRepo(ctx context.Context, req JoinByRepoReq) (JoinByRepoResponse, error)
	GetEvents(ctx context.Context, taskID, since string, limit int) (EventsResponse, error)
	AppendEvent(ctx context.Context, taskID string, req AppendEventReq) (Event, error)
	CreateSprint(ctx context.Context, req CreateSprintReq) (SprintSummary, error)
	UpsertRun(ctx context.Context, sprintID string, req UpsertRunReq) (RunSummary, error)
	Join(ctx context.Context, taskID string, req JoinReq) (JoinResponse, error)
	Lease(ctx context.Context, taskID string, req LeaseReq) (LeaseResponse, error)
	GrantShare(ctx context.Context, taskID string, req GrantShareReq) (TaskShare, error)
	ListShares(ctx context.Context, taskID string) ([]TaskShare, error)
	RevokeShare(ctx context.Context, taskID, sharee string) error
}

type httpSessionClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewHTTPSessionClient(base, token string) *httpSessionClient {
	return &httpSessionClient{
		BaseURL: strings.TrimRight(base, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type TaskSummary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Display      string    `json:"display"`
	Goal         string    `json:"goal"`
	TargetRepo   string    `json:"target_repo,omitempty"`
	DoneCriteria string    `json:"done_criteria"`
	Status       string    `json:"status"`
	Summary      string    `json:"summary"`
	LeaseHolder  *string   `json:"lease_holder"`
	LeaseEpoch   int       `json:"lease_epoch"`
	Created      time.Time `json:"created"`
	Modified     time.Time `json:"modified"`
}

type ListTasksResponse struct {
	Tasks []TaskSummary `json:"tasks"`
}

// RepoSessions is the answer to `?repo=`: what the caller can reach on the
// key, and what it could be seated on by GitHub's say-so.
type RepoSessions struct {
	Repo     string        `json:"repo"`
	Sessions []RepoSession `json:"sessions"`
}

type RepoSession struct {
	Task     TaskSummary `json:"task"`
	Joinable bool        `json:"joinable"`
}

// ErrRepoQueryUnsupported: the server answered the plain list (it ignores
// `?repo=`), so the caller keys the sessions itself.
var ErrRepoQueryUnsupported = errors.New("cloudbox: ?repo= not supported by this server")

type JoinByRepoReq struct {
	Repo        string `json:"repo"`
	Participant string `json:"participant"`
	Host        string `json:"host"`
	Tool        string `json:"tool"`
}

type JoinByRepoResponse struct {
	Task    TaskSummary               `json:"task"`
	Role    string                    `json:"role"`
	Context map[string][]ContextEvent `json:"context"`
	Cursor  string                    `json:"cursor"`
}

// CreateTaskReq mirrors cloudbox's v1CreateTaskReq; a repo-keyed team session
// sets TargetRepo to the normalized origin and nothing else it does not know.
type CreateTaskReq struct {
	Name       string `json:"name,omitempty"`
	Display    string `json:"display,omitempty"`
	Goal       string `json:"goal"`
	TargetRepo string `json:"target_repo,omitempty"`
	// Discovery: "" (derived — a github.com key is joinable by GitHub's
	// say-so) or "private" (owner + explicit shares only).
	Discovery    string   `json:"discovery,omitempty"`
	TargetRef    string   `json:"target_ref,omitempty"`
	Gate         string   `json:"gate,omitempty"`
	Fleet        []string `json:"fleet,omitempty"`
	DoneCriteria string   `json:"done_criteria,omitempty"`
}

type Event struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Summary    string          `json:"summary"`
	Detail     json.RawMessage `json:"detail"`
	Created    time.Time       `json:"created"`
	LeaseEpoch int             `json:"lease_epoch"`
}

type EventsResponse struct {
	Events     []Event `json:"events"`
	Cursor     string  `json:"cursor"`
	LeaseEpoch int     `json:"lease_epoch"`
}

type AppendEventReq struct {
	Kind       string          `json:"kind"`
	Summary    string          `json:"summary"`
	Detail     json.RawMessage `json:"detail"`
	LeaseEpoch *int            `json:"lease_epoch"`
}

type CreateSprintReq struct {
	TargetRepo string   `json:"target_repo"`
	Gate       string   `json:"gate"`
	TaskID     string   `json:"task_id"`
	Fleet      []string `json:"fleet,omitempty"`
}

type SprintSummary struct {
	ID         string   `json:"id"`
	TargetRepo string   `json:"target_repo,omitempty"`
	Gate       string   `json:"gate,omitempty"`
	TaskID     string   `json:"task_id,omitempty"`
	Fleet      []string `json:"fleet,omitempty"`
}

type UpsertRunReq struct {
	Issue        string  `json:"issue"`
	Agent        string  `json:"agent"`
	Host         string  `json:"host,omitempty"`
	Branch       string  `json:"branch,omitempty"`
	Sandbox      string  `json:"sandbox,omitempty"`
	Status       string  `json:"status"`
	CommitsAhead int     `json:"commits_ahead"`
	Exit         int     `json:"exit"`
	Verdict      string  `json:"verdict,omitempty"`
	GateOutput   string  `json:"gate_output,omitempty"`
	LogTail      string  `json:"log_tail,omitempty"`
	TraceID      string  `json:"trace_id,omitempty"`
	TokensIn     int     `json:"tokens_in,omitempty"`
	TokensOut    int     `json:"tokens_out,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
	Tool         string  `json:"tool,omitempty"`
	Model        string  `json:"model,omitempty"`
	Provider     string  `json:"provider,omitempty"`
}

type RunSummary struct {
	ID           string  `json:"id"`
	SprintID     string  `json:"sprint_id,omitempty"`
	Issue        string  `json:"issue"`
	Agent        string  `json:"agent,omitempty"`
	Host         string  `json:"host,omitempty"`
	Branch       string  `json:"branch,omitempty"`
	Sandbox      string  `json:"sandbox,omitempty"`
	Status       string  `json:"status,omitempty"`
	CommitsAhead int     `json:"commits_ahead,omitempty"`
	Exit         int     `json:"exit,omitempty"`
	Verdict      string  `json:"verdict,omitempty"`
	GateOutput   string  `json:"gate_output,omitempty"`
	LogTail      string  `json:"log_tail,omitempty"`
	TraceID      string  `json:"trace_id,omitempty"`
	TokensIn     int     `json:"tokens_in,omitempty"`
	TokensOut    int     `json:"tokens_out,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
	Tool         string  `json:"tool,omitempty"`
	Model        string  `json:"model,omitempty"`
	Provider     string  `json:"provider,omitempty"`
}

type JoinReq struct {
	Participant string `json:"participant"`
	Host        string `json:"host"`
	Tool        string `json:"tool"`
	Role        string `json:"role"`
}

type JoinTask struct {
	ID           string  `json:"id"`
	Goal         string  `json:"goal"`
	DoneCriteria string  `json:"done_criteria"`
	Status       string  `json:"status"`
	Summary      string  `json:"summary"`
	LeaseHolder  *string `json:"lease_holder"`
	LeaseEpoch   int     `json:"lease_epoch"`
}

type ContextEvent struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Summary string          `json:"summary"`
	Detail  json.RawMessage `json:"detail"`
	Created time.Time       `json:"created"`
}

type JoinResponse struct {
	Task    JoinTask                  `json:"task"`
	Context map[string][]ContextEvent `json:"context"`
	Cursor  string                    `json:"cursor"`
}

type LeaseReq struct {
	Action     string `json:"action"`
	Holder     string `json:"holder"`
	TTLSeconds *int   `json:"ttl_seconds,omitempty"`
}

type LeaseResponse struct {
	LeaseHolder  *string   `json:"lease_holder"`
	LeaseEpoch   int       `json:"lease_epoch"`
	LeaseExpires time.Time `json:"lease_expires"`
}

type GrantShareReq struct {
	ShareeEmail string `json:"sharee_email"`
	Role        string `json:"role,omitempty"`
}

type TaskShare struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	ShareeEmail string `json:"sharee_email"`
	Role        string `json:"role"`
	Created     string `json:"created"`
}

type ListSharesResponse struct {
	Shares []TaskShare `json:"shares"`
}

func (c *httpSessionClient) ListTasks(ctx context.Context) ([]TaskSummary, error) {
	var out ListTasksResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks", nil, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

func (c *httpSessionClient) CreateTask(ctx context.Context, req CreateTaskReq) (TaskSummary, error) {
	var out TaskSummary
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks", req, &out); err != nil {
		return TaskSummary{}, err
	}
	return out, nil
}

func (c *httpSessionClient) ListTasksByRepo(ctx context.Context, repo string) (RepoSessions, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks?repo="+url.QueryEscape(repo), nil, &raw); err != nil {
		return RepoSessions{}, err
	}
	var probe struct {
		Sessions *json.RawMessage `json:"sessions"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Sessions == nil {
		return RepoSessions{}, ErrRepoQueryUnsupported
	}
	var out RepoSessions
	if err := json.Unmarshal(raw, &out); err != nil {
		return RepoSessions{}, fmt.Errorf("cloudbox response parse: %w", err)
	}
	return out, nil
}

func (c *httpSessionClient) JoinByRepo(ctx context.Context, req JoinByRepoReq) (JoinByRepoResponse, error) {
	var out JoinByRepoResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/join-by-repo", req, &out); err != nil {
		return JoinByRepoResponse{}, err
	}
	return out, nil
}

func (c *httpSessionClient) GetEvents(ctx context.Context, taskID, since string, limit int) (EventsResponse, error) {
	path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/events"
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out EventsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return EventsResponse{}, err
	}
	return out, nil
}

func (c *httpSessionClient) AppendEvent(ctx context.Context, taskID string, req AppendEventReq) (Event, error) {
	var out Event
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/events", req, &out); err != nil {
		return Event{}, err
	}
	return out, nil
}

func (c *httpSessionClient) CreateSprint(ctx context.Context, req CreateSprintReq) (SprintSummary, error) {
	var out SprintSummary
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/sprints", req, &out); err != nil {
		return SprintSummary{}, err
	}
	return out, nil
}

func (c *httpSessionClient) UpsertRun(ctx context.Context, sprintID string, req UpsertRunReq) (RunSummary, error) {
	var out RunSummary
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/sprints/"+url.PathEscape(sprintID)+"/runs", req, &out); err != nil {
		return RunSummary{}, err
	}
	return out, nil
}

func (c *httpSessionClient) Join(ctx context.Context, taskID string, req JoinReq) (JoinResponse, error) {
	var out JoinResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/join", req, &out); err != nil {
		return JoinResponse{}, err
	}
	return out, nil
}

func (c *httpSessionClient) Lease(ctx context.Context, taskID string, req LeaseReq) (LeaseResponse, error) {
	var out LeaseResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/lease", req, &out); err != nil {
		return LeaseResponse{}, err
	}
	return out, nil
}

func (c *httpSessionClient) GrantShare(ctx context.Context, taskID string, req GrantShareReq) (TaskShare, error) {
	var out TaskShare
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/shares", req, &out); err != nil {
		return TaskShare{}, err
	}
	return out, nil
}

func (c *httpSessionClient) ListShares(ctx context.Context, taskID string) ([]TaskShare, error) {
	var out ListSharesResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/shares", nil, &out); err != nil {
		return nil, err
	}
	return out.Shares, nil
}

func (c *httpSessionClient) RevokeShare(ctx context.Context, taskID, sharee string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/tasks/"+url.PathEscape(taskID)+"/shares/"+url.PathEscape(sharee), nil, nil)
}

func (c *httpSessionClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapCloudboxError(resp.Status, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("cloudbox response parse: %w", err)
	}
	return nil
}

func mapCloudboxError(status string, body []byte) error {
	var er struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &er)
	switch er.Error.Code {
	case "stale_epoch":
		return fmt.Errorf("%w: %s: %s", ErrStaleEpoch, status, strings.TrimSpace(string(body)))
	case "lease_held":
		return fmt.Errorf("%w: %s: %s", ErrLeaseHeld, status, strings.TrimSpace(string(body)))
	default:
		if strings.HasPrefix(status, "403") {
			var sc struct {
				RequiredScope string `json:"required_scope"`
			}
			_ = json.Unmarshal(body, &sc)
			if sc.RequiredScope != "" {
				return fmt.Errorf("%w: this host's cloudbox token lacks %s. A paired outpost token carries the session scopes once cloudbox runs a build with them (tasks:*/sprints:*, Sprint 217) and its boot-time scope catch-up has run; until then set $CLOUDBOX_TOKEN to a token minted with tasks:read,tasks:write,sprints:read,sprints:write", ErrInsufficientScope, sc.RequiredScope)
			}
		}
		return fmt.Errorf("cloudbox request failed: %s: %s", status, strings.TrimSpace(string(body)))
	}
}

// ErrInsufficientScope is a 403 naming the scope the token lacks. It is the
// one refusal a developer can act on without reading cloudbox's JSON.
var ErrInsufficientScope = errors.New("insufficient scope")
