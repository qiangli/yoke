package fleet

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultProxyExchangeTTL is the default lifetime requested when exchanging an
// account token for a host:proxy satellite token.
const DefaultProxyExchangeTTL = "10m"

// SatelliteAppEndpoint is a parsed https://SATELLITE/matrix/h/HOST/app/NAME/...
// endpoint on a satellite.
type SatelliteAppEndpoint struct {
	URL       string // normalized full URL without trailing slash
	Satellite string // scheme://host
	Host      string // HOST segment
	App       string // NAME segment
}

// ParseSatelliteAppURL validates a satellite app route URL
// (https://SATELLITE/matrix/h/HOST/app/NAME/...; http allowed on loopback
// only) and returns its normalized URL, satellite origin, host and app name.
func ParseSatelliteAppURL(raw string) (SatelliteAppEndpoint, error) {
	var zero SatelliteAppEndpoint
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, "\x00\r\n \t") {
		return zero, fmt.Errorf("fleet: empty or invalid satellite app URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return zero, fmt.Errorf("fleet: invalid satellite app URL %q (want https://SATELLITE/matrix/h/HOST/app/NAME/mcp)", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return zero, fmt.Errorf("fleet: satellite app URL %q requires https outside loopback", raw)
		}
	default:
		return zero, fmt.Errorf("fleet: satellite app URL %q must use https", raw)
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return zero, fmt.Errorf("fleet: invalid path in satellite app URL %q", raw)
		}
	}
	if len(parts) < 5 || parts[0] != "matrix" || parts[1] != "h" || parts[3] != "app" {
		return zero, fmt.Errorf("fleet: %q is not a satellite app URL (want https://SATELLITE/matrix/h/HOST/app/NAME/mcp)", raw)
	}
	origin := u.Scheme + "://" + u.Host
	return SatelliteAppEndpoint{
		URL:       origin + "/" + strings.Join(parts, "/"),
		Satellite: origin,
		Host:      parts[2],
		App:       parts[4],
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// URLOrigin returns scheme://host for a base URL, or "" if invalid.
func URLOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// SatelliteTokenSource exchanges an account token for a short-lived satellite
// token (default scope host:proxy, TTL 10m) and refreshes it before expiry.
// Neither the account token nor the exchanged token is ever logged or printed.
type SatelliteTokenSource struct {
	Client SatelliteClient
	Scopes []string         // default ["host:proxy"]
	TTL    string           // default DefaultProxyExchangeTTL ("10m")
	Now    func() time.Time // default time.Now

	mu        sync.Mutex
	token     string
	scopes    []string
	expiresAt time.Time
	refreshAt time.Time
}

func (s *SatelliteTokenSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns a valid satellite token, exchanging a new one when none is
// cached or when the cached token has entered its pre-expiry refresh window.
func (s *SatelliteTokenSource) Token(ctx context.Context) (TokenExchangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		return TokenExchangeResponse{
			Token:     s.token,
			Scopes:    append([]string(nil), s.scopes...),
			ExpiresAt: s.expiresAt,
		}, nil
	}
	return s.exchangeLocked(ctx)
}

// Exchange forces an immediate token exchange and updates the cached token.
func (s *SatelliteTokenSource) Exchange(ctx context.Context) (TokenExchangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchangeLocked(ctx)
}

// Invalidate drops the cached token if it matches staleToken, forcing the next
// Token call to perform a fresh exchange.
func (s *SatelliteTokenSource) Invalidate(staleToken string) {
	if staleToken == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == staleToken {
		s.token = ""
	}
}

func (s *SatelliteTokenSource) exchangeLocked(ctx context.Context) (TokenExchangeResponse, error) {
	scopes := s.Scopes
	if len(scopes) == 0 {
		scopes = []string{"host:proxy"}
	}
	ttl := strings.TrimSpace(s.TTL)
	if ttl == "" {
		ttl = DefaultProxyExchangeTTL
	}
	fetchedAt := s.now()
	res, err := s.Client.Exchange(ctx, scopes, ttl)
	if err != nil {
		return TokenExchangeResponse{}, err
	}
	exp := res.ExpiresAt
	if exp.IsZero() {
		d, err := time.ParseDuration(ttl)
		if err != nil || d <= 0 {
			d = 10 * time.Minute
		}
		exp = fetchedAt.Add(d)
		res.ExpiresAt = exp
	}
	if len(res.Scopes) == 0 {
		res.Scopes = append([]string(nil), scopes...)
	}
	lifetime := exp.Sub(fetchedAt)
	leeway := lifetime / 5
	if leeway > 30*time.Second {
		leeway = 30 * time.Second
	}
	if leeway < 0 {
		leeway = 0
	}
	s.token = res.Token
	s.scopes = append([]string(nil), res.Scopes...)
	s.expiresAt = exp
	s.refreshAt = exp.Add(-leeway)
	return res, nil
}
