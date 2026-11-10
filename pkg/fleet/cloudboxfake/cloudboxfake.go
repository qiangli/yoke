// Package cloudboxfake is the executable form of
// docs/cloudbox-members-attach-wire.md: an httptest cloudbox master plus one
// httptest satellite per seeded satellite, serving the members, pair-once and
// satellite-attach endpoints exactly as that contract states. Wave 1-3 client
// stories (bashy member, outpost satellite attach, paired-host ls --satellite,
// llm peer --circle) code against it and need no live cloudbox.
//
// If this fake and the doc disagree, one of them is wrong — fix both in the
// same change. Nothing here models cloudbox internals; it keeps just enough
// in-memory state to make the wire behave.
//
// Stdlib-only and independent of pkg/fleet, so fleet's own internal tests can
// import it without a cycle.
package cloudboxfake

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scopes named by the contract (§1).
const (
	ScopeMembersRead    = "members:read"
	ScopeMembersWrite   = "members:write"
	ScopeSatellitesRead = "satellites:read"
	ScopeHostsWrite     = "hosts:write"
	ScopeHost           = "host"
	ScopeAttach         = "attach"
	ScopeSatelliteHost  = "satellite-host"
)

// ownerScopes is what an unscoped (legacy) owner token carries.
var ownerScopes = []string{ScopeMembersRead, ScopeMembersWrite, ScopeSatellitesRead, ScopeHostsWrite}

// TTLs from the contract.
const (
	InvitationTTL  = 7 * 24 * time.Hour
	PairCodeTTL    = 15 * time.Minute
	AttachTokenTTL = 10 * time.Minute
	IdempotencyTTL = 24 * time.Hour
	MembershipTTL  = 30 * time.Second // Cache-Control max-age on membership reads
)

// ErrorBody is the error envelope every non-2xx response carries.
type ErrorBody struct {
	Code          string `json:"code"`
	Error         string `json:"error"`
	RequiredScope string `json:"required_scope,omitempty"`
	SatelliteID   string `json:"satellite_id,omitempty"`
	RetryAfter    int    `json:"retry_after,omitempty"`
}

// Member is one directional owner → member row.
type Member struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Circle       string     `json:"circle"`
	Status       string     `json:"status"` // pending | active
	InvitationID string     `json:"invitation_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	AcceptedAt   *time.Time `json:"accepted_at"`

	owner string
}

// Invitation is a pending invite as the invitee sees it.
type Invitation struct {
	ID        string    `json:"id"`
	FromEmail string    `json:"from_email"`
	Circle    string    `json:"circle"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	to       string
	memberID string
	accepted bool
}

// MemberOf is one row of the reverse view.
type MemberOf struct {
	ID         string    `json:"id"`
	OwnerEmail string    `json:"owner_email"`
	Circle     string    `json:"circle"`
	Status     string    `json:"status"`
	Since      time.Time `json:"since"`
}

// Host is a master host record created by identity-only pairing.
type Host struct {
	Host       string    `json:"host"`
	Hostname   string    `json:"hostname,omitempty"`
	OS         string    `json:"os,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	OwnerEmail string    `json:"owner_email"`
	PairedAt   time.Time `json:"paired_at"`
}

// Satellite is the master's satellite record. Attached and Hosts are
// per-caller views: Hosts for owner tokens, Attached for host tokens.
type Satellite struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Endpoint   string   `json:"endpoint"`
	State      string   `json:"state"`
	OwnerEmail string   `json:"owner_email,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
	Attached   *bool    `json:"attached,omitempty"`
}

// Assignment is the host → satellite assignment (S5).
type Assignment struct {
	Host        string     `json:"host"`
	SatelliteID string     `json:"satellite_id"`
	State       string     `json:"state"` // pending | attached
	AssignedAt  time.Time  `json:"assigned_at"`
	AttachedAt  *time.Time `json:"attached_at"`

	owner string
}

// SatelliteHost is a satellite's record of an attached host.
type SatelliteHost struct {
	Host       string    `json:"host"`
	Hostname   string    `json:"hostname,omitempty"`
	OS         string    `json:"os,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	AttachedAt time.Time `json:"attached_at"`
}

type token struct {
	email     string // owner identity (owner, host and attach tokens all resolve to one)
	host      string // host, attach, satellite-host tokens
	satellite string // attach (audience), satellite-host
	scopes    []string
	expires   time.Time
	used      string // attach: body hash of the redeeming request
}

type pairCode struct {
	owner   string
	expires time.Time
	host    string // set on first redemption
	token   string
}

type idemEntry struct {
	bodyHash [32]byte
	status   int
	body     []byte
	at       time.Time
}

type satServer struct {
	sat   *Satellite
	srv   *httptest.Server
	hosts map[string]*SatelliteHost
	token map[string]string // host -> satellite host token
}

// Fake is the in-memory cloudbox. All exported methods are safe for
// concurrent use.
type Fake struct {
	// Master is the master server; its URL is the client's base URL.
	Master *httptest.Server

	mu          sync.Mutex
	now         func() time.Time
	seq         int
	tokens      map[string]*token
	members     map[string]*Member // id -> row
	invitations map[string]*Invitation
	codes       map[string]*pairCode
	hosts       map[string]*Host // host name -> record
	sats        map[string]*satServer
	assignments map[string]*Assignment // host -> assignment
	idem        map[string]*idemEntry
}

// New starts a fake master; it is closed (with every satellite) at t's end.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{
		now:         func() time.Time { return time.Now().UTC().Truncate(time.Second) },
		tokens:      map[string]*token{},
		members:     map[string]*Member{},
		invitations: map[string]*Invitation{},
		codes:       map[string]*pairCode{},
		hosts:       map[string]*Host{},
		sats:        map[string]*satServer{},
		assignments: map[string]*Assignment{},
		idem:        map[string]*idemEntry{},
	}
	f.Master = httptest.NewServer(f.withIdempotency(http.HandlerFunc(f.serveMaster)))
	t.Cleanup(func() {
		f.Master.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sats {
			s.srv.Close()
		}
	})
	return f
}

// SetNow pins the fake's clock (for expiry tests).
func (f *Fake) SetNow(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// URL is the master base URL.
func (f *Fake) URL() string { return f.Master.URL }

// AddOwner seeds an identity and returns an unscoped (legacy) owner token,
// which carries every owner scope.
func (f *Fake) AddOwner(email string) string {
	return f.AddToken(email, ownerScopes...)
}

// AddToken seeds an owner token carrying exactly the given scopes.
func (f *Fake) AddToken(email string, scopes ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := f.nextID("tok")
	f.tokens[tok] = &token{email: strings.ToLower(email), scopes: append([]string(nil), scopes...)}
	return tok
}

// AddSatellite seeds a running satellite owned by ownerEmail, starts its
// httptest server, and returns its id. The record's endpoint is that server.
func (f *Fake) AddSatellite(t testing.TB, ownerEmail, name string) string {
	t.Helper()
	f.mu.Lock()
	id := f.nextID("sat")
	s := &satServer{
		sat:   &Satellite{ID: id, Name: name, State: "running", OwnerEmail: strings.ToLower(ownerEmail)},
		hosts: map[string]*SatelliteHost{},
		token: map[string]string{},
	}
	f.sats[id] = s
	f.mu.Unlock()
	s.srv = httptest.NewServer(f.withIdempotency(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serveSatellite(s, w, r)
	})))
	f.mu.Lock()
	s.sat.Endpoint = s.srv.URL
	f.mu.Unlock()
	return id
}

// SetSatelliteState changes a satellite's state (e.g. "stopped" to exercise
// satellite_unavailable).
func (f *Fake) SetSatelliteState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sats[id]; s != nil {
		s.sat.State = state
	}
}

// --- plumbing ---------------------------------------------------------------

func (f *Fake) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%04d", prefix, f.seq)
}

type apiError struct {
	status int
	body   ErrorBody
}

func errf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, body: ErrorBody{Code: code, Error: fmt.Sprintf(format, args...)}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	if e.body.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(e.body.RetryAfter))
	}
	writeJSON(w, e.status, e.body)
}

// decode reads a JSON body strictly: unknown fields are 400 bad_request. An
// empty body leaves v untouched.
func decode(r *http.Request, v any) *apiError {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errf(400, "bad_request", "read body: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(400, "bad_request", "body: %v", err)
	}
	return nil
}

// auth resolves the Bearer token and checks that it carries one of scopes.
// Caller holds f.mu.
func (f *Fake) auth(r *http.Request, scopes ...string) (*token, *apiError) {
	h := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || raw == "" {
		return nil, errf(401, "unauthorized", "missing Bearer token")
	}
	tok := f.tokens[raw]
	if tok == nil {
		return nil, errf(401, "unauthorized", "invalid token")
	}
	if !tok.expires.IsZero() && !f.now().Before(tok.expires) {
		return nil, errf(401, "unauthorized", "token expired")
	}
	for _, want := range scopes {
		for _, have := range tok.scopes {
			if have == want {
				return tok, nil
			}
		}
	}
	e := errf(403, "insufficient_scope", "token lacks scope %s", strings.Join(scopes, " or "))
	e.body.RequiredScope = scopes[0]
	return nil, e
}

func hasScope(tok *token, s string) bool {
	for _, have := range tok.scopes {
		if have == s {
			return true
		}
	}
	return false
}

// recorder captures a response so it can be replayed for an Idempotency-Key.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }
func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// withIdempotency implements the contract's Idempotency-Key rule for POST:
// same caller + method + path + key + body within 24h replays the original
// response; a different body is 422 idempotency_mismatch.
func (f *Fake) withIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if r.Method != http.MethodPost || key == "" {
			next.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		sum := sha256.Sum256(raw)
		slot := r.Host + "|" + r.Header.Get("Authorization") + "|" + r.URL.Path + "|" + key
		f.mu.Lock()
		e := f.idem[slot]
		if e != nil && f.now().Sub(e.at) > IdempotencyTTL {
			delete(f.idem, slot)
			e = nil
		}
		f.mu.Unlock()
		if e != nil {
			if e.bodyHash != sum {
				writeErr(w, errf(422, "idempotency_mismatch", "Idempotency-Key %q was used with a different body", key))
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(e.status)
			_, _ = w.Write(e.body)
			return
		}
		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if rec.status/100 == 2 {
			f.mu.Lock()
			f.idem[slot] = &idemEntry{bodyHash: sum, status: rec.status, body: rec.buf.Bytes(), at: f.now()}
			f.mu.Unlock()
		}
	})
}

// route matches "a/b/:id/c" style patterns against a path, returning the
// :params in order.
func route(path, pattern string) ([]string, bool) {
	ps := strings.Split(strings.Trim(path, "/"), "/")
	qs := strings.Split(strings.Trim(pattern, "/"), "/")
	if len(ps) != len(qs) {
		return nil, false
	}
	var params []string
	for i, q := range qs {
		if strings.HasPrefix(q, ":") {
			if ps[i] == "" {
				return nil, false
			}
			params = append(params, ps[i])
		} else if q != ps[i] {
			return nil, false
		}
	}
	return params, true
}

// --- master -----------------------------------------------------------------

var circleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (f *Fake) serveMaster(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, m := r.URL.Path, r.Method
	var e *apiError
	switch {
	case p == "/api/v1/members" && m == http.MethodGet:
		e = f.listMembers(w, r)
	case p == "/api/v1/members" && m == http.MethodPost:
		e = f.addMember(w, r)
	case p == "/api/v1/members/of" && m == http.MethodGet:
		e = f.membersOf(w, r)
	case p == "/api/v1/member/invitations" && m == http.MethodGet:
		e = f.listInvitations(w, r)
	case p == "/api/v1/hosts/invites" && m == http.MethodPost:
		e = f.createPairCode(w, r)
	case p == "/api/v1/hosts/pair" && m == http.MethodPost:
		e = f.pair(w, r)
	case p == "/api/v1/hosts/self" && m == http.MethodDelete:
		e = f.unpair(w, r)
	case p == "/api/v1/satellites" && m == http.MethodGet:
		e = f.listSatellites(w, r)
	case p == "/api/v1/satellites/assignment" && m == http.MethodGet:
		e = f.listAssignments(w, r)
	default:
		if ps, ok := route(p, "api/v1/members/:id"); ok && m == http.MethodPatch {
			e = f.patchMember(w, r, ps[0])
		} else if ok && m == http.MethodDelete {
			e = f.deleteMember(w, r, ps[0])
		} else if ps, ok := route(p, "api/v1/member/invitations/:id/accept"); ok && m == http.MethodPost {
			e = f.accept(w, r, ps[0])
		} else if ps, ok := route(p, "api/v1/satellites/:id/attach"); ok && m == http.MethodPost {
			e = f.attach(w, r, ps[0])
		} else if ok && m == http.MethodDelete {
			e = f.detach(w, r, ps[0])
		} else if ps, ok := route(p, "api/v1/satellites/:id"); ok && m == http.MethodGet {
			e = f.showSatellite(w, r, ps[0])
		} else {
			e = errf(404, "not_found", "no route %s %s", m, p)
		}
	}
	if e != nil {
		writeErr(w, e)
	}
}

func (f *Fake) listMembers(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeMembersRead)
	if e != nil {
		return e
	}
	circle := r.URL.Query().Get("circle")
	out := []Member{}
	for _, m := range f.members {
		if m.owner == tok.email && (circle == "" || m.Circle == circle) {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(MembershipTTL.Seconds())))
	writeJSON(w, 200, map[string]any{"members": out})
	return nil
}

func (f *Fake) memberRow(owner, email string) *Member {
	for _, m := range f.members {
		if m.owner == owner && m.Email == email {
			return m
		}
	}
	return nil
}

func (f *Fake) addMember(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	var body struct {
		Email  string `json:"email"`
		Circle string `json:"circle"`
	}
	if e := decode(r, &body); e != nil {
		return e
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if at := strings.Index(email, "@"); at < 1 || at == len(email)-1 {
		return errf(422, "invalid_email", "email %q is not an address", body.Email)
	}
	if body.Circle == "" {
		body.Circle = "default"
	}
	if !circleRE.MatchString(body.Circle) {
		return errf(422, "invalid_circle", "circle %q must match %s", body.Circle, circleRE)
	}
	if email == tok.email {
		return errf(422, "self_member", "cannot add yourself as a member")
	}
	if m := f.memberRow(tok.email, email); m != nil {
		writeJSON(w, 200, map[string]any{"member": m})
		return nil
	}
	now := f.now()
	inv := &Invitation{ID: f.nextID("inv"), FromEmail: tok.email, Circle: body.Circle, CreatedAt: now, ExpiresAt: now.Add(InvitationTTL), to: email}
	m := &Member{ID: f.nextID("mem"), Email: email, Circle: body.Circle, Status: "pending", InvitationID: inv.ID, CreatedAt: now, owner: tok.email}
	inv.memberID = m.ID
	f.members[m.ID] = m
	f.invitations[inv.ID] = inv
	writeJSON(w, 201, map[string]any{"member": m, "invitation": inv})
	return nil
}

func (f *Fake) patchMember(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	var body struct {
		Circle *string `json:"circle"`
	}
	if e := decode(r, &body); e != nil {
		return e
	}
	m := f.members[id]
	if m == nil || m.owner != tok.email {
		return errf(404, "not_found", "member %s not found", id)
	}
	if body.Circle != nil {
		if !circleRE.MatchString(*body.Circle) {
			return errf(422, "invalid_circle", "circle %q must match %s", *body.Circle, circleRE)
		}
		m.Circle = *body.Circle
	}
	writeJSON(w, 200, map[string]any{"member": m})
	return nil
}

func (f *Fake) deleteMember(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	if m := f.members[id]; m != nil && m.owner == tok.email {
		delete(f.members, id)
		delete(f.invitations, m.InvitationID)
	}
	w.WriteHeader(204)
	return nil
}

func (f *Fake) listInvitations(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeMembersRead)
	if e != nil {
		return e
	}
	out := []Invitation{}
	for _, inv := range f.invitations {
		if inv.to == tok.email && !inv.accepted && f.now().Before(inv.ExpiresAt) {
			out = append(out, *inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"invitations": out})
	return nil
}

func (f *Fake) accept(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	var body struct {
		Circle string `json:"circle"`
	}
	if e := decode(r, &body); e != nil {
		return e
	}
	inv := f.invitations[id]
	if inv == nil || inv.to != tok.email || f.members[inv.memberID] == nil {
		return errf(404, "not_found", "invitation %s not found", id)
	}
	if !inv.accepted && !f.now().Before(inv.ExpiresAt) {
		return errf(410, "invitation_expired", "invitation %s expired at %s", id, inv.ExpiresAt.Format(time.RFC3339))
	}
	if body.Circle == "" {
		body.Circle = "default"
	}
	if !circleRE.MatchString(body.Circle) {
		return errf(422, "invalid_circle", "circle %q must match %s", body.Circle, circleRE)
	}
	now := f.now()
	fwd := f.members[inv.memberID]
	if fwd.Status != "active" {
		fwd.Status, fwd.AcceptedAt = "active", &now
	}
	inv.accepted = true
	rev := f.memberRow(tok.email, inv.FromEmail)
	if rev == nil {
		rev = &Member{ID: f.nextID("mem"), Email: inv.FromEmail, Circle: body.Circle, CreatedAt: now, owner: tok.email}
		f.members[rev.ID] = rev
	}
	if rev.Status != "active" {
		rev.Status, rev.AcceptedAt = "active", &now
	}
	writeJSON(w, 200, map[string]any{"member": fwd, "reverse": rev})
	return nil
}

func (f *Fake) membersOf(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeMembersRead)
	if e != nil {
		return e
	}
	out := []MemberOf{}
	for _, m := range f.members {
		if m.Email != tok.email {
			continue
		}
		since := m.CreatedAt
		if m.AcceptedAt != nil {
			since = *m.AcceptedAt
		}
		out = append(out, MemberOf{ID: m.ID, OwnerEmail: m.owner, Circle: m.Circle, Status: m.Status, Since: since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(MembershipTTL.Seconds())))
	writeJSON(w, 200, map[string]any{"of": out})
	return nil
}

func (f *Fake) createPairCode(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeHostsWrite)
	if e != nil {
		return e
	}
	if e := decode(r, &struct{}{}); e != nil {
		return e
	}
	f.seq++
	code := fmt.Sprintf("PAIR-%04d", f.seq)
	exp := f.now().Add(PairCodeTTL)
	f.codes[code] = &pairCode{owner: tok.email, expires: exp}
	writeJSON(w, 201, map[string]any{"code": code, "expires_at": exp})
	return nil
}

func (f *Fake) pair(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Code     string `json:"code"`
		Host     string `json:"host"`
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}
	if e := decode(r, &body); e != nil {
		return e
	}
	if body.Code == "" || body.Host == "" {
		return errf(400, "bad_request", "code and host are required")
	}
	c := f.codes[body.Code]
	if c == nil {
		return errf(401, "invalid_code", "pair code is not valid")
	}
	if !f.now().Before(c.expires) {
		return errf(410, "code_expired", "pair code expired")
	}
	if c.host != "" {
		if c.host == body.Host && f.tokens[c.token] != nil {
			writeJSON(w, 200, map[string]any{"host": f.hosts[c.host], "token": c.token, "scopes": []string{ScopeHost}, "tunnel": false})
			return nil
		}
		return errf(409, "code_consumed", "pair code already used")
	}
	if _, taken := f.hosts[body.Host]; taken {
		return errf(409, "host_exists", "host %q is already paired; unpair it first", body.Host)
	}
	h := &Host{Host: body.Host, Hostname: body.Hostname, OS: body.OS, Arch: body.Arch, OwnerEmail: c.owner, PairedAt: f.now()}
	f.hosts[h.Host] = h
	tk := f.nextID("htok")
	f.tokens[tk] = &token{email: c.owner, host: h.Host, scopes: []string{ScopeHost}}
	c.host, c.token = h.Host, tk
	writeJSON(w, 201, map[string]any{"host": h, "token": tk, "scopes": []string{ScopeHost}, "tunnel": false})
	return nil
}

func (f *Fake) unpair(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeHost)
	if e != nil {
		return e
	}
	if a := f.assignments[tok.host]; a != nil {
		f.dropAttachment(tok.host, a.SatelliteID)
	}
	delete(f.hosts, tok.host)
	for k, t := range f.tokens {
		if t.host == tok.host {
			delete(f.tokens, k)
		}
	}
	w.WriteHeader(204)
	return nil
}

// dropAttachment removes host's assignment to satID, invalidates its
// attach tokens and revokes its satellite host token. The fake revokes
// immediately; the contract allows up to 60s at the satellite.
func (f *Fake) dropAttachment(host, satID string) {
	if a := f.assignments[host]; a != nil && a.SatelliteID == satID {
		delete(f.assignments, host)
	}
	for k, t := range f.tokens {
		if t.host == host && t.satellite == satID {
			delete(f.tokens, k)
		}
	}
	if s := f.sats[satID]; s != nil {
		delete(s.hosts, host)
		delete(s.token, host)
	}
}

// satelliteView renders a satellite record for the caller.
func (f *Fake) satelliteView(s *satServer, tok *token) Satellite {
	v := *s.sat
	if hasScope(tok, ScopeHost) {
		a := f.assignments[tok.host]
		att := a != nil && a.SatelliteID == s.sat.ID && a.State == "attached"
		v.Attached = &att
		return v
	}
	for h, a := range f.assignments {
		if a.SatelliteID == s.sat.ID && a.State == "attached" {
			v.Hosts = append(v.Hosts, h)
		}
	}
	sort.Strings(v.Hosts)
	return v
}

func (f *Fake) visibleSatellite(id string, tok *token) *satServer {
	s := f.sats[id]
	if s == nil || s.sat.OwnerEmail != tok.email {
		return nil
	}
	return s
}

func (f *Fake) listSatellites(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeSatellitesRead, ScopeHost)
	if e != nil {
		return e
	}
	out := []Satellite{}
	for _, s := range f.sats {
		if s.sat.OwnerEmail == tok.email {
			v := f.satelliteView(s, tok)
			if !hasScope(tok, ScopeHost) {
				// The owner list keeps its existing {id,name,endpoint,state} shape.
				v.OwnerEmail, v.Hosts = "", nil
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"satellites": out})
	return nil
}

func (f *Fake) showSatellite(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeSatellitesRead, ScopeHost)
	if e != nil {
		return e
	}
	s := f.visibleSatellite(id, tok)
	if s == nil {
		return errf(404, "not_found", "satellite %s not found", id)
	}
	writeJSON(w, 200, map[string]any{"satellite": f.satelliteView(s, tok)})
	return nil
}

func (f *Fake) listAssignments(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeSatellitesRead, ScopeHost)
	if e != nil {
		return e
	}
	sat := r.URL.Query().Get("satellite")
	out := []Assignment{}
	for h, a := range f.assignments {
		if a.owner != tok.email || (hasScope(tok, ScopeHost) && h != tok.host) || (sat != "" && a.SatelliteID != sat) {
			continue
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	writeJSON(w, 200, map[string]any{"assignments": out})
	return nil
}

func (f *Fake) attach(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeHost)
	if e != nil {
		return e
	}
	if e := decode(r, &struct{}{}); e != nil {
		return e
	}
	s := f.visibleSatellite(id, tok)
	if s == nil {
		return errf(404, "not_found", "satellite %s not found", id)
	}
	if a := f.assignments[tok.host]; a != nil && a.SatelliteID != id {
		e := errf(409, "attached_elsewhere", "host %s is attached to %s; detach first", tok.host, a.SatelliteID)
		e.body.SatelliteID = a.SatelliteID
		return e
	}
	if s.sat.State != "running" {
		return errf(409, "satellite_unavailable", "satellite %s is %s", id, s.sat.State)
	}
	// A new attach token supersedes any unredeemed earlier one.
	for k, t := range f.tokens {
		if t.host == tok.host && hasScope(t, ScopeAttach) && t.used == "" {
			delete(f.tokens, k)
		}
	}
	now := f.now()
	at := f.nextID("atok")
	f.tokens[at] = &token{email: tok.email, host: tok.host, satellite: id, scopes: []string{ScopeAttach}, expires: now.Add(AttachTokenTTL)}
	if a := f.assignments[tok.host]; a == nil {
		f.assignments[tok.host] = &Assignment{Host: tok.host, SatelliteID: id, State: "pending", AssignedAt: now, owner: tok.email}
	}
	writeJSON(w, 201, map[string]any{
		"attach_token": at,
		"expires_at":   now.Add(AttachTokenTTL),
		"satellite":    map[string]string{"id": s.sat.ID, "name": s.sat.Name, "endpoint": s.sat.Endpoint},
		"host":         tok.host,
	})
	return nil
}

func (f *Fake) detach(w http.ResponseWriter, r *http.Request, id string) *apiError {
	tok, e := f.auth(r, ScopeHost)
	if e != nil {
		return e
	}
	f.dropAttachment(tok.host, id)
	w.WriteHeader(204)
	return nil
}

// --- satellite --------------------------------------------------------------

func (f *Fake) serveSatellite(s *satServer, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var e *apiError
	switch {
	case r.URL.Path == "/api/v1/hosts/attach" && r.Method == http.MethodPost:
		e = f.redeemAttach(s, w, r)
	case r.URL.Path == "/api/v1/hosts/self" && r.Method == http.MethodDelete:
		e = f.leaveSatellite(s, w, r)
	default:
		e = errf(404, "not_found", "no route %s %s", r.Method, r.URL.Path)
	}
	if e != nil {
		writeErr(w, e)
	}
}

func (f *Fake) redeemAttach(s *satServer, w http.ResponseWriter, r *http.Request) *apiError {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	h, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok := f.tokens[h]
	if tok == nil || !hasScope(tok, ScopeAttach) || tok.satellite != s.sat.ID {
		return errf(401, "invalid_attach_token", "attach token is not valid for this satellite")
	}
	if !f.now().Before(tok.expires) {
		return errf(410, "attach_token_expired", "attach token expired")
	}
	var body struct {
		Host     string `json:"host"`
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}
	if e := decode(r, &body); e != nil {
		return e
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(raw))
	if tok.used != "" {
		if tok.used != sum || s.hosts[tok.host] == nil {
			return errf(401, "attach_token_used", "attach token already redeemed")
		}
		writeJSON(w, 200, map[string]any{"host": s.hosts[tok.host], "token": s.token[tok.host], "scopes": []string{ScopeSatelliteHost}, "satellite_id": s.sat.ID})
		return nil
	}
	if body.Host != tok.host {
		return errf(422, "host_mismatch", "attach token was issued to host %q", tok.host)
	}
	now := f.now()
	// Re-attach refreshes the satellite host token.
	if old := s.token[tok.host]; old != "" {
		delete(f.tokens, old)
	}
	rec := &SatelliteHost{Host: body.Host, Hostname: body.Hostname, OS: body.OS, Arch: body.Arch, AttachedAt: now}
	st := f.nextID("stok")
	f.tokens[st] = &token{email: tok.email, host: tok.host, satellite: s.sat.ID, scopes: []string{ScopeSatelliteHost}}
	s.hosts[tok.host], s.token[tok.host] = rec, st
	tok.used = sum
	if a := f.assignments[tok.host]; a != nil && a.SatelliteID == s.sat.ID {
		a.State, a.AttachedAt = "attached", &now
	}
	writeJSON(w, 201, map[string]any{"host": rec, "token": st, "scopes": []string{ScopeSatelliteHost}, "satellite_id": s.sat.ID})
	return nil
}

func (f *Fake) leaveSatellite(s *satServer, w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeSatelliteHost)
	if e != nil {
		return e
	}
	if tok.satellite != s.sat.ID {
		return errf(401, "unauthorized", "token is not for this satellite")
	}
	f.dropAttachment(tok.host, s.sat.ID)
	w.WriteHeader(204)
	return nil
}
