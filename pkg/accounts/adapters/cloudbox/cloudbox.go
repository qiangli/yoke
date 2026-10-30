// Package cloudbox discovers outpost/cloudbox profiles as accounts. Profile
// files are read-only; credential values are never read into a record, only a
// profile:<name> reference is kept.
package cloudbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/external/meshagent"
	"github.com/qiangli/yoke/pkg/accounts"
)

const Name = "cloudbox"

// ExecFunc runs the outpost CLI and returns its stdout.
type ExecFunc func(ctx context.Context, args ...string) ([]byte, error)

type Adapter struct {
	ConfigDir string   // matrix config dir; default ${XDG_CONFIG_HOME:-$HOME/.config}/matrix
	CachePath string   // optional last-good snapshot file
	Exec      ExecFunc // injectable seam; default execs outpost via meshagent
	Now       func() time.Time

	mu   sync.Mutex
	last []accounts.Observation
}

func New() *Adapter { return &Adapter{} }

// Register is the single registration line.
func Register(r *accounts.Registry) error { return r.Register(New()) }

func (a *Adapter) Name() string { return Name }
func (a *Adapter) Capabilities() accounts.Capability {
	return accounts.Discover | accounts.Select
}
func (a *Adapter) Verify(context.Context, accounts.Account) (accounts.Observation, error) {
	return accounts.Observation{}, accounts.ErrUnsupported
}
func (a *Adapter) Resources(context.Context, accounts.Account) ([]accounts.ResourceRef, error) {
	return nil, accounts.ErrUnsupported
}
func (a *Adapter) Usage(context.Context, accounts.Account, string) ([]accounts.Metric, error) {
	return nil, accounts.ErrUnsupported
}
func (a *Adapter) Billing(context.Context, accounts.Account, string) ([]accounts.Metric, error) {
	return nil, accounts.ErrUnsupported
}

// Select returns the single connection of the account; ambiguity is an error.
func (a *Adapter) Select(_ context.Context, acc accounts.Account, _ accounts.Operation) (accounts.Selection, error) {
	if len(acc.Connections) != 1 {
		return accounts.Selection{}, accounts.ErrAmbiguous
	}
	c := acc.Connections[0]
	return accounts.Selection{AccountID: acc.ID, ConnectionID: c.ID, Endpoint: c.Endpoint, Profile: c.Profile, CredentialRef: c.CredentialRef, AuthMethod: c.AuthMethod, Username: acc.Username}, nil
}

func (a *Adapter) dir() string {
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "matrix")
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) run(ctx context.Context, args ...string) ([]byte, error) {
	if a.Exec != nil {
		return a.Exec(ctx, args...)
	}
	exe, ok := meshagent.Resolve()
	if !ok {
		return nil, errors.New("outpost not installed")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

type profile struct {
	name, server, host, user, agentName string
	expires                             *time.Time
	auth                                string
}

// strField reads whitelisted string keys only; no other value is retained.
func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case float32:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

func findField(m map[string]any, objKeys []string, fieldKeys ...string) string {
	for _, ok := range objKeys {
		if sub, ok := m[ok].(map[string]any); ok {
			if s := strField(sub, fieldKeys...); s != "" {
				return s
			}
		}
	}
	return strField(m, fieldKeys...)
}

func readProfile(dir, name string) (profile, bool) {
	p := profile{name: name}
	b, err := os.ReadFile(filepath.Join(dir, "agent.json"))
	if err != nil {
		return p, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return p, false
	}

	// Derive endpoint from sidecar if present, else from protocol/server_addr/server_port in agent.json.
	if sb, err := os.ReadFile(filepath.Join(dir, "server")); err == nil {
		sidecar := strings.TrimSpace(strings.SplitN(string(sb), "\n", 2)[0])
		if sidecar != "" {
			p.server = sidecar
		}
	}
	if p.server == "" {
		serverAddr := strField(m, "server_addr")
		if serverAddr != "" {
			if strings.Contains(serverAddr, "://") {
				p.server = serverAddr
			} else {
				protocol := strings.ToLower(strField(m, "protocol"))
				serverPort := intField(m, "server_port")
				scheme := "https"
				switch protocol {
				case "http", "ws", "websocket", "tcp":
					scheme = "http"
				case "https", "wss":
					scheme = "https"
				default:
					if serverPort == 443 {
						scheme = "https"
					} else {
						scheme = "http"
					}
				}
				port := ""
				if serverPort != 0 && !((scheme == "https" && serverPort == 443) || (scheme == "http" && serverPort == 80)) {
					port = fmt.Sprintf(":%d", serverPort)
				}
				p.server = scheme + "://" + serverAddr + port
			}
		}
	}
	if p.server == "" {
		p.server = strField(m, "server", "server_url", "endpoint")
	}
	p.server = strings.TrimRight(p.server, "/")

	p.agentName = strField(m, "agent_name")
	p.host = strField(m, "host", "hostname")
	p.user = strField(m, "user", "username", "owner")
	p.auth = strField(m, "auth_method")
	if t, err := time.Parse(time.RFC3339, strField(m, "expires_at")); err == nil {
		p.expires = &t
	}
	if sb, err := os.ReadFile(filepath.Join(dir, "host")); err == nil && p.host == "" {
		p.host = strings.TrimSpace(strings.SplitN(string(sb), "\n", 2)[0])
	}
	return p, true
}

func (a *Adapter) profiles() (profiles []profile, active string) {
	root := a.dir()
	if b, err := os.ReadFile(filepath.Join(root, ".active")); err == nil {
		active = strings.TrimSpace(string(b))
	}
	if p, ok := readProfile(root, active); ok {
		if p.name == "" {
			p.name = "default"
		}
		profiles = append(profiles, p)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if p, ok := readProfile(filepath.Join(root, "profiles", e.Name()), e.Name()); ok {
			profiles = append(profiles, p)
		}
	}
	if active == "" && len(profiles) > 0 {
		active = profiles[0].name
	}
	return
}

type remote struct{ name, server, user string }

func parseRemotes(b []byte) []remote {
	var raw []map[string]any
	if json.Unmarshal(b, &raw) != nil {
		if idx := bytes.IndexByte(b, '['); idx >= 0 {
			_ = json.Unmarshal(b[idx:], &raw)
		}
	}
	if len(raw) == 0 {
		var wrap struct {
			Remotes []map[string]any `json:"remotes"`
		}
		if json.Unmarshal(b, &wrap) != nil {
			if idx := bytes.IndexByte(b, '{'); idx >= 0 {
				_ = json.Unmarshal(b[idx:], &wrap)
			}
		}
		raw = wrap.Remotes
	}
	var out []remote
	for _, m := range raw {
		r := remote{name: strField(m, "name", "host", "hostname", "id"), server: strings.TrimRight(strField(m, "server", "server_url", "endpoint"), "/"), user: strField(m, "user", "username", "owner")}
		if r.name != "" {
			out = append(out, r)
		}
	}
	return out
}

func classify(err error, out []byte) string {
	s := strings.ToLower(err.Error() + " " + string(out))
	switch {
	case strings.Contains(s, "403"), strings.Contains(s, "forbidden"):
		return "forbidden"
	case strings.Contains(s, "expired"), strings.Contains(s, "401"), strings.Contains(s, "unauthorized"):
		return "credential_expired"
	}
	return "unreachable"
}

func (a *Adapter) Discover(ctx context.Context) ([]accounts.Observation, error) {
	profiles, active := a.profiles()
	now := a.now()

	var lastErr string
	statusOut, err := a.run(ctx, "status", "--json")
	if err != nil {
		lastErr = classify(err, statusOut)
	}
	var remotes []remote
	if rb, rerr := a.run(ctx, "remote", "list", "--json"); rerr != nil {
		if lastErr == "" {
			lastErr = classify(rerr, rb)
		}
	} else {
		remotes = parseRemotes(rb)
	}

	var statusUser, statusAgentName, statusCloudboxURL string
	if lastErr == "" {
		var m map[string]any
		if json.Unmarshal(statusOut, &m) != nil {
			if idx := bytes.IndexByte(statusOut, '{'); idx >= 0 {
				_ = json.Unmarshal(statusOut[idx:], &m)
			}
		}
		if m != nil {
			statusAgentName = findField(m, []string{"status", "config"}, "agent_name")
			if statusAgentName == "" {
				statusAgentName = strField(m, "agent_name")
			}
			statusUser = findField(m, []string{"config", "status"}, "user", "username", "oauth2_email")
			if statusUser == "" {
				statusUser = strField(m, "user", "username")
			}
			statusCloudboxURL = findField(m, []string{"status", "config"}, "cloudbox_url")
			if statusCloudboxURL == "" {
				statusCloudboxURL = strField(m, "cloudbox_url")
			}
		}
	}

	if len(profiles) == 0 && lastErr != "" {
		// Discovery is local-only: a host with no profiles, no previous
		// observation and no reachable outpost (typically not installed) simply
		// has no cloudbox account. That is not an adapter failure.
		if lastErr == "unreachable" && len(a.load()) == 0 {
			return nil, nil
		}
		return a.cached(lastErr)
	}

	var activeEndpoint, activeAgentName string
	for i := range profiles {
		if profiles[i].name == active {
			if profiles[i].agentName == "" && statusAgentName != "" {
				profiles[i].agentName = statusAgentName
			}
			if profiles[i].server == "" && statusCloudboxURL != "" {
				profiles[i].server = strings.TrimRight(statusCloudboxURL, "/")
			}
			activeEndpoint = profiles[i].server
			activeAgentName = profiles[i].agentName
			break
		}
	}
	if activeEndpoint == "" && len(profiles) > 0 {
		activeEndpoint = profiles[0].server
		if activeAgentName == "" {
			activeAgentName = profiles[0].agentName
		}
	}
	if activeAgentName == "" {
		activeAgentName = statusAgentName
	}
	if activeEndpoint == "" && statusCloudboxURL != "" {
		activeEndpoint = strings.TrimRight(statusCloudboxURL, "/")
	}

	byKey := map[string]*accounts.Observation{}
	var order []string
	for _, p := range profiles {
		user := p.user
		if user == "" && p.name == active {
			user = statusUser
		}
		agentName := p.agentName
		if agentName == "" && p.name == active {
			agentName = statusAgentName
		}

		acc := accounts.Account{
			Category: accounts.Cloudbox, Provider: Name, Username: user,
			Name: p.host, Source: "outpost-profile", Provenance: "local-observation",
			Status: accounts.Resolved,
		}

		var key string
		if p.server == "" && agentName == "" {
			acc.Status = accounts.Unresolved
			key = "profile:" + p.name
		} else {
			acc.Status = accounts.Resolved
			if user != "" {
				key = p.server + "|" + user
				acc.Username = user
				if p.host != "" {
					acc.Name = p.host
				} else {
					acc.Name = agentName
				}
				acc.ProviderAccountID = key
			} else {
				acc.Username = ""
				acc.Name = agentName
				if p.server != "" && agentName != "" {
					key = p.server + "|" + agentName
				} else if p.server != "" {
					key = p.server
				} else {
					key = agentName
				}
				acc.ProviderAccountID = key
			}
		}

		conn := accounts.Connection{
			ID: p.name, Endpoint: p.server, Profile: p.name,
			CredentialRef: "profile:" + p.name, AuthMethod: p.auth, ExpiresAt: p.expires,
		}
		if p.name == active {
			conn.Scopes = []string{"active"}
		}
		o, ok := byKey[key]
		if !ok {
			t := now
			acc.ObservedAt = &t
			o = &accounts.Observation{Account: acc, SourceKey: key}
			byKey[key] = o
			order = append(order, key)
		}
		o.Account.Connections = append(o.Account.Connections, conn)
		if p.expires != nil && p.expires.Before(now) {
			o.Account.LastError = "credential_expired"
		}
		if p.host != "" {
			o.Account.Links.Hosts = appendUniq(o.Account.Links.Hosts, p.host)
		}
		for _, r := range remotes {
			if r.server == "" && p.name != active {
				continue
			}
			if r.server != "" && r.server != p.server {
				continue
			}
			if r.user != "" && r.user != user {
				continue
			}
			o.Account.Links.Satellites = appendUniq(o.Account.Links.Satellites, r.name)
		}
	}

	// Link satellite profiles: a profile whose endpoint differs from the default/active
	// cloudbox endpoint is a satellite instance of the same user/host; link it via
	// Links.Satellites on the account for the active endpoint ONLY when the agent_name matches;
	// otherwise keep it its own account. Do not invent links.
	for _, p := range profiles {
		if p.server == "" || activeEndpoint == "" || p.server == activeEndpoint {
			continue
		}
		pAgentName := p.agentName
		if pAgentName == "" && p.name == active {
			pAgentName = statusAgentName
		}
		if pAgentName == "" || pAgentName != activeAgentName {
			continue
		}
		for _, o := range byKey {
			isCurrentEndpoint := false
			for _, c := range o.Account.Connections {
				if c.Endpoint == activeEndpoint {
					isCurrentEndpoint = true
					break
				}
			}
			if !isCurrentEndpoint {
				continue
			}
			if o.Account.Username != "" && p.user != "" && o.Account.Username != p.user {
				continue
			}
			o.Account.Links.Satellites = appendUniq(o.Account.Links.Satellites, p.name)
		}
	}

	var out []accounts.Observation
	for _, k := range order {
		o := byKey[k]
		sort.Strings(o.Account.Links.Satellites)
		if lastErr != "" && o.Account.LastError == "" {
			o.Account.LastError = lastErr
		}
		if o.Account.LastError != "" {
			o.Account.Status = accounts.Error
		}
		out = append(out, *o)
	}
	if lastErr != "" {
		// Daemon unreachable: overlay satellites from the last good snapshot.
		out = a.mergeCached(out)
		a.store(out)
		recordProven(ctx, out)
		return out, nil
	}
	a.store(out)
	recordProven(ctx, out)
	return out, nil
}

// recordProven reports to the account inventory what the outpost CLI just
// proved. A completed `outpost status` IS the login check for the active
// profile, so a profile that answered records its identity and one whose
// credential expired or was rejected records the transition instead — in both
// cases keeping whatever the inventory already knew about the account.
//
// Only the profile's own connection travels with the fact: the full set of
// connections belongs to the observation this function's caller returns.
func recordProven(ctx context.Context, obs []accounts.Observation) {
	for _, o := range obs {
		acc := o.Account
		if acc.ProviderAccountID == "" || len(acc.Connections) == 0 {
			// An unresolved profile names no identity; inventing one from a
			// config file is exactly what discovery must not do.
			continue
		}
		c := acc.Connections[0]
		for _, cand := range acc.Connections {
			for _, scope := range cand.Scopes {
				if scope == "active" {
					c = cand
				}
			}
		}
		f := accounts.Facts{
			Adapter: Name, Provider: Name, ProviderAccountID: acc.ProviderAccountID,
			Name: acc.Name, Username: acc.Username, ConnectionID: c.ID,
			Endpoint: c.Endpoint, Profile: c.Profile, CredentialRef: c.CredentialRef,
			AuthMethod: c.AuthMethod, Scopes: c.Scopes, ExpiresAt: c.ExpiresAt,
			Links: acc.Links, Source: "outpost-profile",
		}
		if acc.ObservedAt != nil {
			f.At = *acc.ObservedAt
		}
		if acc.LastError == "" {
			accounts.ObserveSuccess(ctx, accounts.Cloudbox, f)
			continue
		}
		accounts.ObserveTransition(ctx, accounts.Cloudbox, f, accounts.Error, acc.LastError)
	}
}

func appendUniq(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func (a *Adapter) store(obs []accounts.Observation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = obs
	if a.CachePath == "" {
		return
	}
	for _, o := range obs {
		if o.Account.LastError != "" {
			return // only last-good snapshots are persisted
		}
	}
	if b, err := json.Marshal(obs); err == nil {
		_ = os.MkdirAll(filepath.Dir(a.CachePath), 0o700)
		_ = os.WriteFile(a.CachePath, b, 0o600)
	}
}

func (a *Adapter) load() []accounts.Observation {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last != nil {
		return a.last
	}
	if a.CachePath != "" {
		if b, err := os.ReadFile(a.CachePath); err == nil {
			var obs []accounts.Observation
			if json.Unmarshal(b, &obs) == nil {
				return obs
			}
		}
	}
	return nil
}

func (a *Adapter) mergeCached(cur []accounts.Observation) []accounts.Observation {
	prev := a.load()
	idx := map[string]accounts.Observation{}
	for _, p := range prev {
		idx[p.SourceKey] = p
	}
	for i := range cur {
		if p, ok := idx[cur[i].SourceKey]; ok {
			for _, s := range p.Account.Links.Satellites {
				cur[i].Account.Links.Satellites = appendUniq(cur[i].Account.Links.Satellites, s)
			}
			sort.Strings(cur[i].Account.Links.Satellites)
		}
	}
	return cur
}

// cached returns the last-good snapshot marked with LastError.
func (a *Adapter) cached(code string) ([]accounts.Observation, error) {
	prev := a.load()
	if len(prev) == 0 {
		return nil, errors.New(code)
	}
	out := make([]accounts.Observation, len(prev))
	copy(out, prev)
	for i := range out {
		out[i].Account.LastError = code
		out[i].Account.Status = accounts.Error
	}
	return out, nil
}
