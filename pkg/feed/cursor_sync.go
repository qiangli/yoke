package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// syncTimeout bounds client requests to cloudbox feed endpoints.
const syncTimeout = 5 * time.Second

// SyncReadRequest is the POST /api/v1/feed/read request body.
type SyncReadRequest struct {
	IDs []string `json:"ids"`
}

// SyncReadResponse is the 200 response body from POST /api/v1/feed/read.
type SyncReadResponse struct {
	Marked int `json:"marked"`
}

// serverReadChunkSize is the max ids per GET /api/v1/feed/read call.
const serverReadChunkSize = 500

// ServerReadResponse is the 200 body of GET /api/v1/feed/read?ids=....
type ServerReadResponse struct {
	Read  []string `json:"read"`
	Inbox string   `json:"inbox"`
}

// SyncRead sends post IDs to the paired cloudbox server's POST /api/v1/feed/read endpoint.
// When the host is not paired, SyncRead is a silent no-op.
func SyncRead(ctx context.Context, ids []string) error {
	return SyncReadEnv(ctx, Env{}, ids)
}

// SyncReadEnv POSTs ids using the given environment for discovery and HTTP transport.
func SyncReadEnv(ctx context.Context, env Env, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token := cloud()
	if token == "" {
		// Unpaired host: silent no-op.
		return nil
	}
	if base == "" {
		base = "https://ai.dhnt.io"
	}

	body, err := json.Marshal(SyncReadRequest{IDs: ids})
	if err != nil {
		return fmt.Errorf("feed: encoding sync read request: %w", err)
	}

	u := strings.TrimRight(base, "/") + "/api/v1/feed/read"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feed: sync read request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: syncTimeout}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("feed: sync read request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("feed: reading sync read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		reason := serverReason(raw)
		if reason != "" {
			return fmt.Errorf("feed: sync read refused (HTTP %d): %s", resp.StatusCode, reason)
		}
		return fmt.Errorf("feed: sync read refused (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// cloudOf resolves the paired outpost base URL and bearer token.
func cloudOf(env Env) (base, token string) {
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token = cloud()
	if base == "" {
		base = "https://ai.dhnt.io"
	}
	return base, token
}

// uniqIDs drops blanks and duplicates, keeping first-seen order.
func uniqIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// queryServerRead asks GET /api/v1/feed/read?ids=... for one chunk of ids.
func queryServerRead(ctx context.Context, env Env, base, token string, ids []string) (ServerReadResponse, error) {
	var out ServerReadResponse
	u, err := url.Parse(strings.TrimRight(base, "/") + "/api/v1/feed/read")
	if err != nil {
		return out, fmt.Errorf("feed: server read request: %w", err)
	}
	q := u.Query()
	for _, id := range ids {
		q.Add("ids", id)
	}
	u.RawQuery = q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, fmt.Errorf("feed: server read request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: syncTimeout}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("feed: server read request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("feed: reading server read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		reason := serverReason(raw)
		if reason != "" {
			return out, fmt.Errorf("feed: server read refused (HTTP %d): %s", resp.StatusCode, reason)
		}
		return out, fmt.Errorf("feed: server read refused (HTTP %d)", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("feed: decoding server read response: %w", err)
	}
	return out, nil
}

// FetchServerRead asks the paired server which of ids are read, in chunks of
// serverReadChunkSize, and returns exactly the server's "read" set plus the
// reader's web inbox URL. When the host is not paired it makes no request
// and returns a nil map with no error.
func FetchServerRead(ctx context.Context, env Env, ids []string) (map[string]bool, string, error) {
	base, token := cloudOf(env)
	if token == "" {
		// Unpaired host: silent no-op.
		return nil, "", nil
	}
	uniq := uniqIDs(ids)
	read := make(map[string]bool, len(uniq))
	if len(uniq) == 0 {
		return read, "", nil
	}
	var inbox string
	for start := 0; start < len(uniq); start += serverReadChunkSize {
		end := start + serverReadChunkSize
		if end > len(uniq) {
			end = len(uniq)
		}
		res, err := queryServerRead(ctx, env, base, token, uniq[start:end])
		if err != nil {
			return nil, "", err
		}
		for _, id := range res.Read {
			if id != "" {
				read[id] = true
			}
		}
		if inbox == "" && res.Inbox != "" {
			inbox = res.Inbox
		}
	}
	return read, inbox, nil
}

// ServerReadSet returns exactly the post IDs the paired server reports as
// read for the spooled candidate posts. Only IDs the server names are read;
// older unread posts and locally-followed posts never appear in the set.
func ServerReadSet(ctx context.Context) (map[string]bool, error) {
	return ServerReadSetEnv(ctx, Env{})
}

// ServerReadSetEnv returns the server's read set using the given environment.
// When the host is not paired it returns a nil map with no error.
func ServerReadSetEnv(ctx context.Context, env Env) (map[string]bool, error) {
	read, _, err := ServerReadAndInboxEnv(ctx, env)
	return read, err
}

// ServerReadAndInboxEnv queries GET /api/v1/feed/read with the spool's
// candidate post IDs and returns exactly the server's "read" set plus the
// reader's web inbox URL. When the host is not paired it returns a nil map,
// an empty inbox, and no error.
func ServerReadAndInboxEnv(ctx context.Context, env Env) (map[string]bool, string, error) {
	entries, err := ReadSpool(env.dir())
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	return FetchServerRead(ctx, env, ids)
}

// ServerReadFunc returns a predicate reporting the server's read set:
// read(id) is true only for IDs the server named as read.
func ServerReadFunc(ctx context.Context, env Env) (func(string) bool, error) {
	read, err := ServerReadSetEnv(ctx, env)
	if err != nil {
		return nil, err
	}
	return func(id string) bool {
		return read[id]
	}, nil
}

// NewServerReadFunc returns a func(context.Context) (map[string]bool, error)
// constructor suitable for setting as env.serverRead in the deliver command.
func NewServerReadFunc(env Env) func(context.Context) (map[string]bool, error) {
	return func(ctx context.Context) (map[string]bool, error) {
		return ServerReadSetEnv(ctx, env)
	}
}

// NewServerReadAndInbox returns the default deliver wiring for env.serverRead
// and env.inboxURL: both values come from one shared GET /api/v1/feed/read
// query per chunk, so deliver makes one round trip per chunk, not two. A
// failed query is not cached, so the next call retries. The returned funcs
// are not safe for concurrent use; deliver calls them sequentially.
func NewServerReadAndInbox(env Env) (func(context.Context) (map[string]bool, error), func(context.Context) string) {
	var done bool
	var read map[string]bool
	var inbox string
	var err error
	fetch := func(ctx context.Context) (map[string]bool, string, error) {
		if !done {
			read, inbox, err = ServerReadAndInboxEnv(ctx, env)
			if err == nil {
				done = true
			}
		}
		return read, inbox, err
	}
	serverRead := func(ctx context.Context) (map[string]bool, error) {
		r, _, e := fetch(ctx)
		return r, e
	}
	inboxURL := func(ctx context.Context) string {
		_, in, _ := fetch(ctx)
		return in
	}
	return serverRead, inboxURL
}
