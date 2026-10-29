package accounts

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookBudget bounds one observation. A completed operation reports its facts
// on the way out; it must not wait on the inventory, so the hook takes a small
// fixed budget and gives up rather than queueing behind a long-running lock.
const hookBudget = 2 * time.Second

// maxFactLength bounds one recorded field. Inventory metadata is short by
// nature; a long value is a sign that something other than metadata arrived.
const maxFactLength = 512

var errHookUnidentified = errors.New("accounts: observation names no identity and no source key")

// Facts is what a completed operation PROVED, and nothing more. Every field is
// metadata: a credential reference is accepted and validated, a credential
// value never is. Callers fill only what the operation actually established —
// an empty field leaves whatever was already known in place.
type Facts struct {
	// Adapter names what proved the facts. It keys unresolved records and is
	// recorded as provenance; it defaults to the category.
	Adapter string
	// SourceKey identifies a record that proves no identity of its own. It
	// must be stable within one adapter and must never be derived from a
	// person's name or email address.
	SourceKey string

	Provider          string
	ProviderAccountID string
	Name              string
	Username          string
	Email             string
	Team              string
	Org               string
	Project           string
	Tenant            string
	Role              string
	Region            string
	Environment       string
	Lane              string

	ConnectionID  string
	Endpoint      string
	Profile       string
	CredentialRef string
	AuthMethod    string
	Scopes        []string
	ExpiresAt     *time.Time

	Resources []ResourceRef
	SSHKeys   []SSHKeyRef
	Links     Links

	Source string
	At     time.Time
}

var hookMu sync.RWMutex
var hookStore *Store

// SetHookStore points the observation hook at one inventory. Hosts that keep
// the inventory outside BASHY_HOME set it once at startup; nil restores the
// default location. It is the only knob the hook has: callers of
// ObserveSuccess never choose a path.
func SetHookStore(s *Store) {
	hookMu.Lock()
	defer hookMu.Unlock()
	hookStore = s
}

// realHome is captured before any test scaffolding can redirect HOME, so the
// test guard still recognizes the operator's own home.
var realHome, realHomeErr = os.UserHomeDir()

// insideRealHome reports a path inside the operator's live ~/.bashy. Under go
// test the hook refuses those writes: an unfenced test records nothing instead
// of quietly adding accounts to the real inventory. Tests that mean to
// exercise the hook pass their own store through SetHookStore.
func insideRealHome(p string) bool {
	if realHomeErr != nil || realHome == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(realHome, ".bashy"), p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hookTarget() *Store {
	hookMu.RLock()
	s := hookStore
	hookMu.RUnlock()
	if s != nil {
		return s
	}
	p := DefaultPath()
	if p == "" || (testing.Testing() && insideRealHome(p)) {
		return &Store{}
	}
	return Open(p)
}

// ObserveSuccess records the identity and resource relationships that one
// successful operation proved. It is best-effort and deliberately returns
// nothing: an operation that worked must report success even when the
// inventory cannot be written, so a failure here is logged at debug level and
// dropped. Suppressed accounts are never re-added, repeat calls are
// idempotent, and no secret is ever recorded.
func ObserveSuccess(ctx context.Context, category Category, f Facts) {
	hookTarget().ObserveSuccess(ctx, category, f)
}

// ObserveTransition records a status change the caller proved: a logout, an
// expired credential, a revoked token. The last good snapshot is kept — only
// the status and error code move — so a lost credential never erases what the
// account is. Like ObserveSuccess it never reports an error to its caller.
func ObserveTransition(ctx context.Context, category Category, f Facts, status Status, code string) {
	hookTarget().ObserveTransition(ctx, category, f, status, code)
}

// ObserveSuccess records one proven observation into this inventory.
func (s *Store) ObserveSuccess(ctx context.Context, category Category, f Facts) {
	s.observe(ctx, category, f, Resolved, "")
}

// ObserveTransition records one proven status change into this inventory.
func (s *Store) ObserveTransition(ctx context.Context, category Category, f Facts, status Status, code string) {
	s.observe(ctx, category, f, status, code)
}

func (s *Store) observe(ctx context.Context, category Category, f Facts, status Status, code string) {
	// The caller's operation already succeeded. Nothing in here — not a
	// corrupt store, not a bug of ours — may change that outcome, so the
	// panic path is contained here too, where every entry point passes.
	defer func() {
		if v := recover(); v != nil {
			slog.Debug("accounts: observation panicked", "category", string(category), "panic", v)
		}
	}()
	if err := s.record(ctx, category, f, status, code); err != nil {
		slog.Debug("accounts: observation not recorded", "category", string(category), "status", string(status), "error", err)
	}
}

// record is the error-returning core of the hook. Only tests read the error;
// every production caller goes through observe, which swallows it.
func (s *Store) record(ctx context.Context, category Category, f Facts, status Status, code string) error {
	switch status {
	case Resolved, Unresolved, Error:
	default:
		// Suppression is an operator act through Remove, never an observation.
		return errors.New("accounts: invalid observed status")
	}
	if code != "" && !errorCodePattern.MatchString(code) {
		return errors.New("accounts: last_error must be a safe code")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()

	adapter := safeFact(f.Adapter)
	if adapter == "" {
		adapter = string(category)
	}
	a := f.account(category)
	a.Status = status
	a.LastError = code
	a.Provenance = adapter
	if err := validateAccount(a); err != nil {
		return err
	}
	key, ok := identityKey(a)
	if !ok {
		sourceKey := safeFact(f.SourceKey)
		if sourceKey == "" {
			return errHookUnidentified
		}
		key = unresolvedKey(a, adapter, sourceKey)
	}
	a.ID = localID(key)
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := hookBudget
	if deadline, has := ctx.Deadline(); has {
		wait = time.Until(deadline)
	}
	return s.mutateWithin(wait, func(d *document) error {
		for i := range d.Records {
			if d.Records[i].Key != key {
				continue
			}
			if d.Records[i].Suppressed {
				// A removed account stays removed: automatic collection must
				// not undo an operator's decision on every next operation.
				return nil
			}
			d.Records[i].Observation = mergeObserved(d.Records[i].Observation, a)
			return checkAliases(d)
		}
		d.Records = append(d.Records, record{Key: key, Observation: a})
		return checkAliases(d)
	})
}

// account builds the candidate record. Fields arrive from live operations, so
// each one is length-bounded and stripped of control characters, and a
// credential reference that is not a reference is dropped rather than stored.
func (f Facts) account(category Category) Account {
	at := f.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	a := Account{
		Category:          category,
		Provider:          safeFact(f.Provider),
		ProviderAccountID: safeFact(f.ProviderAccountID),
		Name:              safeFact(f.Name),
		Username:          safeFact(f.Username),
		Email:             safeFact(f.Email),
		Team:              safeFact(f.Team),
		Org:               safeFact(f.Org),
		Project:           safeFact(f.Project),
		Tenant:            safeFact(f.Tenant),
		Role:              safeFact(f.Role),
		Region:            safeFact(f.Region),
		Environment:       safeFact(f.Environment),
		Lane:              safeFact(f.Lane),
		Source:            safeFact(f.Source),
		ObservedAt:        &at,
	}
	if a.Provider == "" {
		// Provider is part of every key and the store requires it. A caller
		// that names only a category gets the category as its provider rather
		// than having the whole observation dropped.
		a.Provider = string(category)
	}
	c := Connection{
		ID:            safeFact(f.ConnectionID),
		Endpoint:      safeFact(f.Endpoint),
		Profile:       safeFact(f.Profile),
		CredentialRef: safeRef(f.CredentialRef),
		AuthMethod:    safeFact(f.AuthMethod),
		Scopes:        safeFactList(f.Scopes),
		ExpiresAt:     f.ExpiresAt,
	}
	if c.ID == "" {
		c.ID = c.Profile
	}
	if c.ID == "" {
		c.ID = c.Endpoint
	}
	if c.ID != "" || c.Endpoint != "" || c.Profile != "" || c.CredentialRef != "" || c.AuthMethod != "" || len(c.Scopes) > 0 || c.ExpiresAt != nil {
		a.Connections = []Connection{c}
	}
	for _, r := range f.Resources {
		r.ID, r.Kind, r.Name, r.Source = safeFact(r.ID), safeFact(r.Kind), safeFact(r.Name), safeFact(r.Source)
		if r.ID != "" && r.Kind != "" {
			a.Resources = append(a.Resources, r)
		}
	}
	for _, k := range f.SSHKeys {
		k.Fingerprint, k.Name, k.CredentialRef = safeFact(k.Fingerprint), safeFact(k.Name), safeRef(k.CredentialRef)
		if k.Fingerprint != "" {
			a.SSHKeys = append(a.SSHKeys, k)
		}
	}
	a.Links = Links{
		Satellites: safeFactList(f.Links.Satellites),
		Hosts:      safeFactList(f.Links.Hosts),
		Models:     safeFactList(f.Links.Models),
		Agents:     safeFactList(f.Links.Agents),
	}
	return a
}

// safeFact keeps a short, single-line metadata value and drops anything else.
func safeFact(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > maxFactLength {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

// safeRef keeps a credential REFERENCE and drops anything that is not one, so
// a caller that passes a token value by mistake stores nothing instead of
// writing the value into the inventory.
func safeRef(v string) string {
	v = safeFact(v)
	if v == "" || !ValidCredentialRef(v) {
		return ""
	}
	return v
}

func safeFactList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = safeFact(v); v != "" {
			out = appendFact(out, v)
		}
	}
	return out
}

func appendFact(out []string, v string) []string {
	for _, have := range out {
		if have == v {
			return out
		}
	}
	return append(out, v)
}

// mergeObserved folds one proven observation into the stored one. A single
// operation proves what it touched and nothing about the rest, so known fields
// win, absent fields keep their last good value, and lists are unioned rather
// than replaced. The observation timestamp moves only when something else
// did — that is what makes a retried or repeated operation a no-op down to the
// bytes on disk.
func mergeObserved(old, next Account) Account {
	out := old
	out.ID = next.ID
	out.Category = next.Category
	out.Status = next.Status
	out.LastError = next.LastError
	out.Provenance = next.Provenance
	mergeField(&out.Provider, next.Provider)
	mergeField(&out.ProviderAccountID, next.ProviderAccountID)
	mergeField(&out.Name, next.Name)
	mergeField(&out.Username, next.Username)
	mergeField(&out.Email, next.Email)
	mergeField(&out.Team, next.Team)
	mergeField(&out.Org, next.Org)
	mergeField(&out.Project, next.Project)
	mergeField(&out.Tenant, next.Tenant)
	mergeField(&out.Role, next.Role)
	mergeField(&out.Region, next.Region)
	mergeField(&out.Environment, next.Environment)
	mergeField(&out.Lane, next.Lane)
	mergeField(&out.Source, next.Source)
	out.Connections = mergeConnections(old.Connections, next.Connections)
	out.Resources = mergeResources(old.Resources, next.Resources)
	out.SSHKeys = mergeSSHKeys(old.SSHKeys, next.SSHKeys)
	out.Links = Links{
		Satellites: mergeList(old.Links.Satellites, next.Links.Satellites),
		Hosts:      mergeList(old.Links.Hosts, next.Links.Hosts),
		Models:     mergeList(old.Links.Models, next.Links.Models),
		Agents:     mergeList(old.Links.Agents, next.Links.Agents),
	}
	out.ObservedAt = old.ObservedAt
	if !reflect.DeepEqual(out, old) {
		out.ObservedAt = next.ObservedAt
	}
	return out
}

func mergeField(dst *string, src string) {
	if src != "" {
		*dst = src
	}
}

func mergeList(old, next []string) []string {
	out := append([]string(nil), old...)
	for _, v := range next {
		out = appendFact(out, v)
	}
	return out
}

// mergeConnections upserts the connection an operation used. A hook synthesizes
// its connection id, so identity falls back to endpoint and profile and then to
// the credential reference: a repeated connect has to land on the row the
// operator entered by hand instead of appending a near-duplicate beside it.
func sameConnection(have, c Connection) bool {
	if c.Endpoint != "" || c.Profile != "" {
		return have.Endpoint == c.Endpoint && have.Profile == c.Profile
	}
	if c.ID != "" {
		return have.ID == c.ID
	}
	return c.CredentialRef != "" && have.CredentialRef == c.CredentialRef
}

func mergeConnections(old, next []Connection) []Connection {
	out := append([]Connection(nil), old...)
	for _, c := range next {
		idx := -1
		for i := range out {
			if out[i].ID == c.ID || sameConnection(out[i], c) {
				idx = i
				break
			}
		}
		if idx < 0 {
			out = append(out, c)
			continue
		}
		mergeField(&out[idx].Endpoint, c.Endpoint)
		mergeField(&out[idx].Profile, c.Profile)
		mergeField(&out[idx].CredentialRef, c.CredentialRef)
		mergeField(&out[idx].AuthMethod, c.AuthMethod)
		out[idx].Scopes = mergeList(out[idx].Scopes, c.Scopes)
		if c.ExpiresAt != nil {
			out[idx].ExpiresAt = c.ExpiresAt
		}
	}
	return out
}

func mergeResources(old, next []ResourceRef) []ResourceRef {
	out := append([]ResourceRef(nil), old...)
	for _, r := range next {
		found := false
		for i := range out {
			if out[i].Kind == r.Kind && out[i].ID == r.ID {
				mergeField(&out[i].Name, r.Name)
				mergeField(&out[i].Source, r.Source)
				found = true
				break
			}
		}
		if !found {
			out = append(out, r)
		}
	}
	return out
}

func mergeSSHKeys(old, next []SSHKeyRef) []SSHKeyRef {
	out := append([]SSHKeyRef(nil), old...)
	for _, k := range next {
		found := false
		for i := range out {
			if out[i].Fingerprint == k.Fingerprint {
				mergeField(&out[i].Name, k.Name)
				mergeField(&out[i].CredentialRef, k.CredentialRef)
				found = true
				break
			}
		}
		if !found {
			out = append(out, k)
		}
	}
	return out
}
