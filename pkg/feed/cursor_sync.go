package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// FeedUnreadResponse represents the GET /api/v1/feed?unread=1 response.
type FeedUnreadResponse struct {
	Timeline string     `json:"timeline"`
	Posts    []FeedItem `json:"posts"`
	Next     string     `json:"next"`
}

// FeedItem is an item returned in the feed timeline.
type FeedItem struct {
	ID string `json:"id"`
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

// FetchServerUnread calls GET /api/v1/feed?unread=1 and returns the set of unread
// post IDs reported by the server. If the host is not paired, it returns an empty
// set and nil error.
func FetchServerUnread(ctx context.Context, env Env) (map[string]bool, error) {
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token := cloud()
	if token == "" {
		// Unpaired host: silent no-op.
		return map[string]bool{}, nil
	}
	if base == "" {
		base = "https://ai.dhnt.io"
	}

	u := strings.TrimRight(base, "/") + "/api/v1/feed?unread=1"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("feed: server unread request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: syncTimeout}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("feed: server unread request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("feed: reading server unread response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		reason := serverReason(raw)
		if reason != "" {
			return nil, fmt.Errorf("feed: server unread refused (HTTP %d): %s", resp.StatusCode, reason)
		}
		return nil, fmt.Errorf("feed: server unread refused (HTTP %d)", resp.StatusCode)
	}

	var out FeedUnreadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("feed: decoding server unread response: %w", err)
	}

	unread := make(map[string]bool, len(out.Posts))
	for _, p := range out.Posts {
		if p.ID != "" {
			unread[p.ID] = true
		}
	}
	return unread, nil
}

// ServerReadSet returns post IDs that are known posts but NOT in the unread list.
//
// Design choice:
// ServerReadSet fetches unread post IDs via GET /api/v1/feed?unread=1 and compares
// against known posts in the local spool (ReadSpool): any known spooled post not
// present in the unread map is deemed read (readSet[id] = true).
// Callers who already hold their candidate post IDs can also use ServerReadFunc,
// which returns a predicate func(id string) bool defined as read(id) = !unread[id].
func ServerReadSet(ctx context.Context) (map[string]bool, error) {
	return ServerReadSetEnv(ctx, Env{})
}

// ServerReadSetEnv returns the set of read post IDs using the given environment.
func ServerReadSetEnv(ctx context.Context, env Env) (map[string]bool, error) {
	unread, err := FetchServerUnread(ctx, env)
	if err != nil {
		return nil, err
	}
	entries, err := ReadSpool(env.dir())
	if err != nil {
		return nil, err
	}
	readSet := make(map[string]bool)
	for _, e := range entries {
		if e.ID != "" && !unread[e.ID] {
			readSet[e.ID] = true
		}
	}
	return readSet, nil
}

// ServerReadFunc returns a predicate function where read(id) = !unread[id] for
// IDs known to the server.
func ServerReadFunc(ctx context.Context, env Env) (func(string) bool, error) {
	unread, err := FetchServerUnread(ctx, env)
	if err != nil {
		return nil, err
	}
	return func(id string) bool {
		return !unread[id]
	}, nil
}

// NewServerReadFunc returns a func(context.Context) (map[string]bool, error)
// constructor suitable for setting as env.serverRead in the deliver command.
func NewServerReadFunc(env Env) func(context.Context) (map[string]bool, error) {
	return func(ctx context.Context) (map[string]bool, error) {
		return ServerReadSetEnv(ctx, env)
	}
}
