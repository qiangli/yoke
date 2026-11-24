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

const proxyExchangeTTL = "10m"

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
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return zero, fmt.Errorf("invalid satellite app URL (want https://SATELLITE/matrix/h/HOST/app/NAME/mcp)")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return zero, fmt.Errorf("satellite app URL requires https outside loopback")
	}
	path := strings.TrimSuffix(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 7 || parts[0] != "" || parts[1] != "matrix" || parts[2] != "h" || parts[4] != "app" || parts[6] != "mcp" ||
		strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
		return zero, fmt.Errorf("expected satellite app route /matrix/h/HOST/app/NAME/mcp")
	}
	for _, segment := range []string{parts[3], parts[5]} {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\\x00\r\n") {
			return zero, fmt.Errorf("invalid host or app in satellite app URL")
		}
	}
	u.Path, u.RawPath = path, ""
	return SatelliteAppEndpoint{URL: u.String(), Satellite: u.Scheme + "://" + u.Host, Host: parts[3], App: parts[5]}, nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
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
	Client    SatelliteClient
	Now       func() time.Time // optional clock for expiry
	mu        sync.Mutex
	token     string
	refreshAt time.Time
}

// Token returns the cached host:proxy bearer or exchanges the account token.
func (s *SatelliteTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	start := now()
	if start.Before(s.refreshAt) && s.token != "" {
		return s.token, nil
	}
	res, err := s.Client.Exchange(ctx, []string{"host:proxy"}, proxyExchangeTTL)
	if err != nil {
		return "", err
	}
	lifetime := res.ExpiresAt.Sub(start)
	if lifetime <= 0 {
		return "", fmt.Errorf("satellite exchange returned no future expiry")
	}
	s.token = res.Token
	s.refreshAt = res.ExpiresAt.Add(-min(30*time.Second, lifetime/5))
	return s.token, nil
}
