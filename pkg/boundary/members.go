// Package boundary checks explicit grants against the owner's Cloudbox member list.
package boundary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Member is the owner's directional row. Mutual is computed by the master.
type Member struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name,omitempty"`
	Status       string `json:"status"`
	Circle       string `json:"circle"`
	Mutual       bool   `json:"mutual"`
	InvitationID string `json:"invitation_id,omitempty"`
}

type Invitation struct {
	ID        string    `json:"id"`
	FromEmail string    `json:"from_email"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

type MemberOf struct {
	ID         string    `json:"id"`
	OwnerEmail string    `json:"owner_email"`
	Status     string    `json:"status"`
	Mutual     bool      `json:"mutual"`
	Since      time.Time `json:"since"`
}

// Client uses one owner credential. A successful list is retained for offline
// display; authorization callers must inspect the stale result before use.
type Client struct {
	BaseURL, Token string
	HTTP           *http.Client
	TTL            time.Duration
	Now            func() time.Time
	mu             sync.Mutex
	rows           []Member
	fetched        time.Time
	known          bool
}

// OfflineAuthorizationMaxAge is how long an unreachable master may be
// replaced by the last successfully fetched member list for an authorization
// decision. Authentication and protocol failures never use the snapshot.
const OfflineAuthorizationMaxAge = 24 * time.Hour

// Decision records the membership result and the list used to make it.
type Decision struct {
	Allowed bool
	Offline bool
	ListAge time.Duration
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func (c *Client) ttl() time.Duration {
	if c.TTL > 0 && c.TTL <= 30*time.Second {
		return c.TTL
	}
	return 30 * time.Second
}

type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudbox: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

func (c *Client) Do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, data)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Code, Error string }
		_ = json.Unmarshal(raw, &e)
		return nil, &APIError{resp.StatusCode, e.Code, e.Error}
	}
	return raw, nil
}

// List returns the whole owner list and whether it came from the last known
// snapshot. An empty list is retained too. Failed first reads return an error.
func (c *Client) List(ctx context.Context) ([]Member, bool, error) {
	rows, stale, _, err := c.ListWithAge(ctx)
	return rows, stale, err
}

// ListWithAge is List plus the age of the successful master response backing
// the rows. Age is zero immediately after a successful refresh.
func (c *Client) ListWithAge(ctx context.Context) ([]Member, bool, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	age := now.Sub(c.fetched)
	if age < 0 {
		age = 0
	}
	if c.known && age < c.ttl() {
		return append([]Member{}, c.rows...), false, age, nil
	}
	raw, err := c.Do(ctx, http.MethodGet, "/api/v1/members", nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return nil, false, 0, err
		}
		if c.known {
			return append([]Member{}, c.rows...), true, age, nil
		}
		return nil, false, 0, err
	}
	var env struct {
		Members []Member `json:"members"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, 0, fmt.Errorf("members: decode: %w", err)
	}
	if env.Members == nil {
		return nil, false, 0, fmt.Errorf("members: missing members list")
	}
	c.rows, c.fetched, c.known = append([]Member{}, env.Members...), now, true
	return append([]Member{}, c.rows...), false, 0, nil
}

func (c *Client) Invalidate() { c.mu.Lock(); c.fetched = time.Time{}; c.mu.Unlock() }

// Allowed accepts an exact email or circle grant only for an active, mutual
// member. A removed row is absent and therefore cannot match.
func Allowed(email string, grants []string, members []Member) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, m := range members {
		if strings.ToLower(m.Email) != email || m.Status != "active" || !m.Mutual {
			continue
		}
		for _, g := range grants {
			if strings.EqualFold(g, email) || g == m.Circle {
				return true
			}
		}
	}
	return false
}

// Decide checks membership against the master, or its last known list while
// the master is unreachable and that list is no more than 24 hours old.
func (c *Client) Decide(ctx context.Context, email string, grants []string) (Decision, error) {
	rows, stale, age, err := c.ListWithAge(ctx)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Allowed: Allowed(email, grants, rows), Offline: stale, ListAge: age}
	if stale && age > OfflineAuthorizationMaxAge {
		return d, fmt.Errorf("members: last known list is %s old (maximum 24h); refusing", age.Round(time.Second))
	}
	return d, nil
}

// Allowed is the compatibility form for callers that need only the result.
func (c *Client) Allowed(ctx context.Context, email string, grants []string) (bool, error) {
	d, err := c.Decide(ctx, email, grants)
	return d.Allowed, err
}

func MemberPath(id string) string { return "/api/v1/members/" + url.PathEscape(id) }
func InvitationPath(id string) string {
	return "/api/v1/member/invitations/" + url.PathEscape(id) + "/accept"
}
