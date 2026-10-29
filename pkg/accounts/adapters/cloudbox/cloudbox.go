// Package cloudbox discovers outpost/cloudbox profiles as accounts. Profile
// files are read-only; credential values are never read into a record, only a
// profile:<name> reference is kept.
package cloudbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	name, server, host, user string
	expires                  *time.Time
	auth                     string
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
	p.server = strField(m, "server", "server_url", "endpoint")
	p.host = strField(m, "host", "hostname")
	p.user = strField(m, "user", "username", "owner")
	p.auth = strField(m, "auth_method")
	if t, err := time.Parse(time.RFC3339, strField(m, "expires_at")); err == nil {
		p.expires = &t
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"server", &p.server}, {"host", &p.host}} {
		if sb, err := os.ReadFile(filepath.Join(dir, f.name)); err == nil && *f.dst == "" {
			*f.dst = strings.TrimSpace(strings.SplitN(string(sb), "\n", 2)[0])
		}
	}
	p.server = strings.TrimRight(p.server, "/")
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
	return
}

type remote struct{ name, server, user string }

func parseRemotes(b []byte) []remote {
	var raw []map[string]any
	if json.Unmarshal(b, &raw) != nil {
		var wrap struct {
			Remotes []map[string]any `json:"remotes"`
		}
		if json.Unmarshal(b, &wrap) != nil {
			return nil
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
	var statusUser string
	if lastErr == "" {
		var m map[string]any
		if json.Unmarshal(statusOut, &m) == nil {
			statusUser = strField(m, "user", "username")
		}
	}

	if len(profiles) == 0 && lastErr != "" {
		return a.cached(lastErr)
	}

	byKey := map[string]*accounts.Observation{}
	var order []string
	for _, p := range profiles {
		user := p.user
		if user == "" && p.name == active {
			user = statusUser
		}
		acc := accounts.Account{
			Category: accounts.Cloudbox, Provider: Name, Username: user,
			Name: p.host, Source: "outpost-profile", Provenance: "local-observation",
			Status: accounts.Resolved,
		}
		key := p.server + "|" + user
		if p.server == "" || user == "" {
			acc.Status = accounts.Unresolved
			key = "profile:" + p.name
		} else {
			acc.ProviderAccountID = key
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
		return out, nil
	}
	a.store(out)
	return out, nil
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
