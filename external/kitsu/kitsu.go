// Package kitsu provides a bounded client for a caller-managed Zou API.
// It never starts or configures a Zou server and never acquires credentials.
package kitsu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrMissingCredential       = errors.New("kitsu: caller token required")
	ErrPlaintextHTTPNotAllowed = errors.New("kitsu: HTTP requires a loopback fixture")
	ErrCrossOriginRedirect     = errors.New("kitsu: cross-origin redirect rejected")
	ErrExecutionNotExplicit    = errors.New("kitsu: write execution mode required")
	ErrUnsupported             = errors.New("kitsu: operation has no verified endpoint")
	ErrBodyTooLarge            = errors.New("kitsu: response body too large")
	ErrInvalidID               = errors.New("kitsu: invalid UUID")
	ErrInvalidComment          = errors.New("kitsu: invalid comment text")
	ErrCommentTooLarge         = errors.New("kitsu: comment text too large")
)

const (
	DefaultTimeout            = 15 * time.Second
	MaxBodyBytes        int64 = 4 << 20
	MaxCommentBytes           = 16 << 10
	MaxRequestBodyBytes       = 20 << 10
)

// Config contains only caller-supplied, process-local configuration.
type Config struct {
	BaseURL    string // Origin root or /api; remote origins must use HTTPS.
	Token      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type Client struct {
	base    *url.URL
	token   string
	http    *http.Client
	timeout time.Duration
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Token != "" && (strings.TrimSpace(cfg.Token) == "" || strings.ContainsAny(cfg.Token, "\r\n")) {
		return nil, ErrMissingCredential
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/" && u.Path != "/api" && u.Path != "/api/") {
		return nil, errors.New("kitsu: invalid API base URL")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, ErrPlaintextHTTPNotAllowed
	}
	u.Path = "/api"
	timeout := cfg.Timeout
	if timeout <= 0 || timeout > DefaultTimeout {
		timeout = DefaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// A private copy enforces our redirect policy even for an injected client.
	copyClient := *httpClient
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, u) {
			return ErrCrossOriginRedirect
		}
		if len(via) >= 5 {
			return errors.New("kitsu: too many redirects")
		}
		return nil
	}
	return &Client{base: u, token: cfg.Token, http: &copyClient, timeout: timeout}, nil
}

// ReadOperation is the complete allowlist of verified GET operations.
type ReadOperation uint8

const (
	ListProjects ReadOperation = iota + 1
	ListEpisodes
	ListSequences
	ListShots
	ListTasks
	ListShotVersions
	ListShotPreviews
	ListTaskPreviews
	ListTaskComments
)

// Query holds the documented project-task filters. Empty values are omitted.
type Query struct {
	Name       string // Project list name filter.
	Page       int    // Project tasks page; zero means server default.
	EpisodeID  string
	TaskTypeID string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validID(id string) bool { return uuidPattern.MatchString(id) }

func readRoute(op ReadOperation, id string, q Query) (string, error) {
	var path string
	switch op {
	case ListProjects:
		path = "/data/projects/all"
	case ListEpisodes, ListSequences, ListShots, ListTasks:
		if !validID(id) {
			return "", ErrInvalidID
		}
		noun := map[ReadOperation]string{ListEpisodes: "episodes", ListSequences: "sequences", ListShots: "shots", ListTasks: "tasks"}[op]
		path = "/data/projects/" + id + "/" + noun
	case ListShotVersions, ListShotPreviews:
		if !validID(id) {
			return "", ErrInvalidID
		}
		noun := "versions"
		if op == ListShotPreviews {
			noun = "preview-files"
		}
		path = "/data/shots/" + id + "/" + noun
	case ListTaskPreviews, ListTaskComments:
		if !validID(id) {
			return "", ErrInvalidID
		}
		noun := "previews"
		if op == ListTaskComments {
			noun = "comments"
		}
		path = "/data/tasks/" + id + "/" + noun
	default:
		return "", ErrUnsupported
	}
	values := url.Values{}
	if op == ListProjects && q.Name != "" {
		values.Set("name", q.Name)
	}
	if op == ListTasks {
		if q.Page < 0 {
			return "", errors.New("kitsu: negative page")
		}
		if q.Page > 0 {
			values.Set("page", fmt.Sprint(q.Page))
		}
		if q.EpisodeID != "" {
			if !validID(q.EpisodeID) {
				return "", ErrInvalidID
			}
			values.Set("episode_id", q.EpisodeID)
		}
		if q.TaskTypeID != "" {
			if !validID(q.TaskTypeID) {
				return "", ErrInvalidID
			}
			values.Set("task_type_id", q.TaskTypeID)
		}
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path, nil
}

// Read returns JSON from a verified endpoint; callers decode the server's
// version-specific record shape. The operation enum prevents arbitrary routes.
func (c *Client) Read(ctx context.Context, op ReadOperation, id string, q Query) (json.RawMessage, error) {
	path, err := readRoute(op, id, q)
	if err != nil {
		return nil, err
	}
	body, _, err := c.request(ctx, http.MethodGet, path, nil)
	return body, err
}

func (c *Client) request(ctx context.Context, method, path string, payload []byte) (json.RawMessage, int, error) {
	if c.token == "" {
		return nil, 0, ErrMissingCredential
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := *c.base
	parts := strings.SplitN(path, "?", 2)
	u.Path += parts[0]
	if len(parts) == 2 {
		u.RawQuery = parts[1]
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrCrossOriginRedirect) {
			return nil, 0, ErrCrossOriginRedirect
		}
		// net/http errors can include URLs; never return one that could contain a secret.
		return nil, 0, errors.New("kitsu: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("kitsu: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("kitsu: response read failed")
	}
	if int64(len(body)) > MaxBodyBytes {
		return nil, resp.StatusCode, ErrBodyTooLarge
	}
	if !json.Valid(body) {
		return nil, resp.StatusCode, errors.New("kitsu: invalid JSON response")
	}
	return json.RawMessage(body), resp.StatusCode, nil
}

// WriteOption requires the caller to choose an execution mode explicitly.
type WriteOption uint8

const (
	DryRun WriteOption = iota + 1
	Execute
)

func WithDryRun() WriteOption  { return DryRun }
func WithExecute() WriteOption { return Execute }

type Effect struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	BodySummary *CommentBodySummary `json:"body_summary,omitempty"`
}
type CommentBodySummary struct {
	TaskStatusID string `json:"task_status_id"`
	CommentBytes int    `json:"comment_bytes"`
}
type WriteResult struct {
	Effect   Effect `json:"effect"`
	Executed bool   `json:"executed"`
}

// DeleteComment is the verified DELETE /data/tasks/{task}/comments/{comment}.
// Dry runs do not access the network. A successful 204 counts as executed.
func (c *Client) DeleteComment(ctx context.Context, taskID, commentID string, options ...WriteOption) (WriteResult, error) {
	if !validID(taskID) || !validID(commentID) {
		return WriteResult{}, ErrInvalidID
	}
	effect := Effect{Method: http.MethodDelete, Path: "/api/data/tasks/" + taskID + "/comments/" + commentID}
	result := WriteResult{Effect: effect}
	if len(options) != 1 || (options[0] != DryRun && options[0] != Execute) {
		return result, ErrExecutionNotExplicit
	}
	if options[0] == DryRun {
		return result, nil
	}
	_, _, err := c.request(ctx, http.MethodDelete, strings.TrimPrefix(effect.Path, "/api"), nil)
	if err != nil {
		return result, err
	}
	result.Executed = true
	return result, nil
}

// CreateComment posts a task comment and its required task status. The status
// change is a server-side effect of creating the comment in Zou.
func (c *Client) CreateComment(ctx context.Context, taskID, taskStatusID, comment string, options ...WriteOption) (WriteResult, error) {
	if !validID(taskID) || !validID(taskStatusID) {
		return WriteResult{}, ErrInvalidID
	}
	if !utf8.ValidString(comment) {
		return WriteResult{}, ErrInvalidComment
	}
	if len(comment) > MaxCommentBytes {
		return WriteResult{}, ErrCommentTooLarge
	}
	effect := Effect{
		Method:      http.MethodPost,
		Path:        "/api/actions/tasks/" + taskID + "/comment",
		BodySummary: &CommentBodySummary{TaskStatusID: taskStatusID, CommentBytes: len(comment)},
	}
	result := WriteResult{Effect: effect}
	if len(options) != 1 || (options[0] != DryRun && options[0] != Execute) {
		return result, ErrExecutionNotExplicit
	}
	payload, err := json.Marshal(struct {
		TaskStatusID string `json:"task_status_id"`
		Comment      string `json:"comment"`
	}{TaskStatusID: taskStatusID, Comment: comment})
	if err != nil || len(payload) > MaxRequestBodyBytes {
		return result, ErrCommentTooLarge
	}
	if options[0] == DryRun {
		return result, nil
	}
	_, status, err := c.request(ctx, http.MethodPost, strings.TrimPrefix(effect.Path, "/api"), payload)
	if err != nil {
		return result, err
	}
	if status != http.StatusCreated {
		return result, fmt.Errorf("kitsu: unexpected HTTP %d", status)
	}
	result.Executed = true
	return result, nil
}

// ReviewAction is deliberately unavailable: the inspected Zou tree did not
// establish a stable, typed review mutation contract for this client.
func (c *Client) ReviewAction(context.Context, string, ...WriteOption) (WriteResult, error) {
	return WriteResult{}, ErrUnsupported
}
