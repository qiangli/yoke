package cloudboxfake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type call struct {
	t    *testing.T
	base string
}

// do sends one request and decodes the JSON response into a generic map.
func (c call) do(method, path, tok string, body any, hdr ...string) (int, map[string]any, http.Header) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

func (c call) want(status int, method, path, tok string, body any, hdr ...string) map[string]any {
	c.t.Helper()
	got, out, _ := c.do(method, path, tok, body, hdr...)
	if got != status {
		c.t.Fatalf("%s %s: status %d, want %d (body %v)", method, path, got, status, out)
	}
	return out
}

func (c call) wantErr(status int, code, method, path, tok string, body any) {
	c.t.Helper()
	out := c.want(status, method, path, tok, body)
	if out["code"] != code || out["error"] == "" {
		c.t.Fatalf("%s %s: envelope %v, want code %q", method, path, out, code)
	}
}

func obj(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }
func list(m map[string]any, k string) []any         { v, _ := m[k].([]any); return v }
func str(m map[string]any, k string) string         { v, _ := m[k].(string); return v }

func TestCircleNamesOwnerDefined(t *testing.T) {
	f := New(t)
	alice, bob := f.AddOwner("alice@example.com"), f.AddOwner("bob@example.com")
	m := call{t, f.URL()}

	// Reserved and malformed names fail closed with an actionable reason.
	for _, circle := range []string{"public", "owner", "Bad Circle", "team:ops", "UPPER", "under_score", "a23456789012345678901234567890123"} {
		m.wantErr(422, "invalid_circle", "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": circle})
	}
	got := m.want(422, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "public"})
	if msg := str(got, "error"); !strings.Contains(msg, "reserved") || !strings.Contains(msg, "family") {
		t.Fatalf("reserved error not actionable: %q", msg)
	}

	// A custom circle round-trips: invite, set, ls filter.
	add := m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "hiking-club"})
	mem := obj(add, "member")
	if str(mem, "circle") != "hiking-club" {
		t.Fatalf("custom circle member = %v", mem)
	}
	if got := list(m.want(200, "GET", "/api/v1/members?circle=hiking-club", alice, nil), "members"); len(got) != 1 {
		t.Fatalf("ls circle=hiking-club = %v", got)
	}
	p := m.want(200, "PATCH", "/api/v1/members/"+str(mem, "id"), alice, map[string]string{"circle": "x1"})
	if str(obj(p, "member"), "circle") != "x1" {
		t.Fatalf("patch to custom circle = %v", p)
	}
	if got := list(m.want(200, "GET", "/api/v1/members?circle=hiking-club", alice, nil), "members"); len(got) != 0 {
		t.Fatalf("ls old circle after set = %v", got)
	}
	// The invitee files the reverse row under their own custom circle.
	acc := m.want(200, "POST", "/api/v1/member/invitations/"+str(obj(add, "invitation"), "id")+"/accept", bob, map[string]string{"circle": "work-2026"})
	if rev := obj(acc, "reverse"); str(rev, "circle") != "work-2026" {
		t.Fatalf("reverse row custom circle = %v", rev)
	}
	m.wantErr(422, "invalid_circle", "PATCH", "/api/v1/members/"+str(mem, "id"), alice, map[string]string{"circle": "owner"})
}

func TestMembersMutualAccept(t *testing.T) {
	f := New(t)
	alice, bob := f.AddOwner("alice@example.com"), f.AddOwner("bob@example.com")
	m := call{t, f.URL()}

	m.wantErr(401, "unauthorized", "GET", "/api/v1/members", "", nil)
	ro := f.AddToken("alice@example.com", ScopeMembersRead)
	m.wantErr(403, "insufficient_scope", "POST", "/api/v1/members", ro, map[string]string{"email": "bob@example.com"})
	m.wantErr(422, "invalid_circle", "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "Bad Circle"})
	m.wantErr(422, "invalid_circle", "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "team:ops"})
	m.wantErr(422, "self_member", "POST", "/api/v1/members", alice, map[string]string{"email": "alice@example.com"})
	m.wantErr(400, "bad_request", "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circel": "x"})

	add := m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "Bob@example.com", "circle": "family"})
	mem, inv := obj(add, "member"), obj(add, "invitation")
	if str(mem, "status") != "pending" || str(mem, "circle") != "family" || str(mem, "email") != "bob@example.com" || mem["mutual"] != false {
		t.Fatalf("member = %v", mem)
	}
	// Natural idempotency: same email is 200 and does not change the circle.
	again := obj(m.want(200, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "team"}), "member")
	if str(again, "id") != str(mem, "id") || str(again, "circle") != "family" {
		t.Fatalf("re-add = %v", again)
	}

	invs := list(m.want(200, "GET", "/api/v1/member/invitations", bob, nil), "invitations")
	if len(invs) != 1 || str(invs[0].(map[string]any), "from_email") != "alice@example.com" || str(invs[0].(map[string]any), "status") != "pending" {
		t.Fatalf("invitations = %v", invs)
	}
	if _, leaked := invs[0].(map[string]any)["circle"]; leaked {
		t.Fatalf("invitation discloses the inviter's circle: %v", invs[0])
	}
	// Someone else cannot accept it, and cannot even see it exists.
	carol := f.AddOwner("carol@example.com")
	m.wantErr(404, "not_found", "POST", "/api/v1/member/invitations/"+str(inv, "id")+"/accept", carol, nil)

	acc := m.want(200, "POST", "/api/v1/member/invitations/"+str(inv, "id")+"/accept", bob, map[string]string{"circle": "friend"})
	if fwd := obj(acc, "member"); str(fwd, "status") != "active" || fwd["mutual"] != true {
		t.Fatalf("forward row = %v", obj(acc, "member"))
	}
	rev := obj(acc, "reverse")
	if str(rev, "email") != "alice@example.com" || str(rev, "circle") != "friend" || str(rev, "status") != "active" || rev["mutual"] != true {
		t.Fatalf("reverse row = %v", rev)
	}
	acc2 := m.want(200, "POST", "/api/v1/member/invitations/"+str(inv, "id")+"/accept", bob, map[string]string{"circle": "team"})
	if str(obj(acc2, "reverse"), "id") != str(rev, "id") || str(obj(acc2, "reverse"), "circle") != "friend" {
		t.Fatalf("replayed accept changed the reverse row: %v", acc2)
	}

	_, of, hdr := m.do("GET", "/api/v1/members/of", bob, nil)
	rows := list(of, "of")
	if len(rows) != 1 || str(rows[0].(map[string]any), "owner_email") != "alice@example.com" || rows[0].(map[string]any)["mutual"] != true {
		t.Fatalf("members/of = %v", of)
	}
	if _, leaked := rows[0].(map[string]any)["circle"]; leaked {
		t.Fatalf("members/of discloses the owner's circle: %v", rows[0])
	}
	if hdr.Get("Cache-Control") != "private, max-age=30" {
		t.Fatalf("members/of Cache-Control = %q", hdr.Get("Cache-Control"))
	}
	if got := list(m.want(200, "GET", "/api/v1/members?circle=friend", bob, nil), "members"); len(got) != 1 {
		t.Fatalf("bob members?circle=friends = %v", got)
	}

	p := m.want(200, "PATCH", "/api/v1/members/"+str(mem, "id"), alice, map[string]string{"circle": "team"})
	if str(obj(p, "member"), "circle") != "team" {
		t.Fatalf("patch = %v", p)
	}
	m.wantErr(404, "not_found", "PATCH", "/api/v1/members/"+str(mem, "id"), bob, map[string]string{"circle": "x"})

	m.want(204, "DELETE", "/api/v1/members/"+str(mem, "id"), alice, nil)
	m.want(204, "DELETE", "/api/v1/members/"+str(mem, "id"), alice, nil)
	if got := list(m.want(200, "GET", "/api/v1/members", alice, nil), "members"); len(got) != 0 {
		t.Fatalf("alice members after delete = %v", got)
	}
	// Each side owns its list: bob's reverse row survives, but deleting
	// either row ends the membership.
	got := list(m.want(200, "GET", "/api/v1/members", bob, nil), "members")
	if len(got) != 1 || got[0].(map[string]any)["mutual"] != false {
		t.Fatalf("bob members after alice delete = %v", got)
	}
}

func TestMemberLeavesByDeletingReverseRow(t *testing.T) {
	f := New(t)
	alice, bob := f.AddOwner("alice@example.com"), f.AddOwner("bob@example.com")
	m := call{t, f.URL()}
	add := m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com"})
	if c := str(obj(add, "member"), "circle"); c != DefaultCircle {
		t.Fatalf("default circle = %q, want %q", c, DefaultCircle)
	}
	rev := obj(m.want(200, "POST", "/api/v1/member/invitations/"+str(obj(add, "invitation"), "id")+"/accept", bob, nil), "reverse")
	if str(rev, "circle") != DefaultCircle {
		t.Fatalf("reverse row circle = %v, want guest", rev)
	}
	m.want(204, "DELETE", "/api/v1/members/"+str(rev, "id"), bob, nil)
	if got := list(m.want(200, "GET", "/api/v1/members", alice, nil), "members"); len(got) != 1 || got[0].(map[string]any)["mutual"] != false {
		t.Fatalf("alice members after bob leaves = %v", got)
	}
	if rows := list(m.want(200, "GET", "/api/v1/members/of", bob, nil), "of"); len(rows) != 1 || rows[0].(map[string]any)["mutual"] != false {
		t.Fatalf("bob members/of after leaving = %v", rows)
	}
}

// pairHost pairs a host for owner and returns its host token.
func pairHost(m call, owner, host string) string {
	m.t.Helper()
	code := str(m.want(201, "POST", "/api/v1/hosts/invites", owner, nil), "code")
	return str(m.want(201, "POST", "/api/v1/hosts/pair", "", map[string]string{"code": code, "host": host}), "token")
}

func TestCrossOwnerAttachRequiresMutual(t *testing.T) {
	f := New(t)
	alice, bob := f.AddOwner("alice@example.com"), f.AddOwner("bob@example.com")
	sat := f.AddSatellite(t, "alice@example.com", "zoo")
	m := call{t, f.URL()}
	bobHost := pairHost(m, bob, "bobbox")

	// No membership, then a pending one: the satellite does not exist for bob.
	m.wantErr(404, "not_found", "POST", "/api/v1/satellites/"+sat+"/attach", bobHost, nil)
	inv := obj(m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com", "circle": "friend"}), "invitation")
	m.wantErr(404, "not_found", "POST", "/api/v1/satellites/"+sat+"/attach", bobHost, nil)
	if sats := list(m.want(200, "GET", "/api/v1/satellites", bobHost, nil), "satellites"); len(sats) != 0 {
		t.Fatalf("pending member sees satellites: %v", sats)
	}

	m.want(200, "POST", "/api/v1/member/invitations/"+str(inv, "id")+"/accept", bob, nil)
	at := m.want(201, "POST", "/api/v1/satellites/"+sat+"/attach", bobHost, nil)
	s := call{t, str(obj(at, "satellite"), "endpoint")}
	s.want(201, "POST", "/api/v1/hosts/attach", str(at, "attach_token"), map[string]string{"host": "bobbox"})
	// The satellite's owner sees the attachment of a host that is not theirs.
	as := list(m.want(200, "GET", "/api/v1/satellites/assignment?satellite="+sat, alice, nil), "assignments")
	if len(as) != 1 || str(as[0].(map[string]any), "host") != "bobbox" || str(as[0].(map[string]any), "state") != "attached" {
		t.Fatalf("satellite owner's assignments = %v", as)
	}
	// Ending the membership (either row) detaches the cross-owner host.
	mine := list(m.want(200, "GET", "/api/v1/members", bob, nil), "members")
	m.want(204, "DELETE", "/api/v1/members/"+str(mine[0].(map[string]any), "id"), bob, nil)
	if as := list(m.want(200, "GET", "/api/v1/satellites/assignment", alice, nil), "assignments"); len(as) != 0 {
		t.Fatalf("assignments after membership ended = %v", as)
	}
	m.wantErr(404, "not_found", "POST", "/api/v1/satellites/"+sat+"/attach", bobHost, nil)
}

func TestInvitationExpires(t *testing.T) {
	f := New(t)
	alice, bob := f.AddOwner("alice@example.com"), f.AddOwner("bob@example.com")
	m := call{t, f.URL()}
	inv := obj(m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "bob@example.com"}), "invitation")
	later := time.Now().Add(InvitationTTL + time.Hour)
	f.SetNow(func() time.Time { return later })
	m.wantErr(410, "invitation_expired", "POST", "/api/v1/member/invitations/"+str(inv, "id")+"/accept", bob, nil)
}

func TestIdempotencyKey(t *testing.T) {
	f := New(t)
	alice := f.AddOwner("alice@example.com")
	m := call{t, f.URL()}
	a := m.want(201, "POST", "/api/v1/hosts/invites", alice, nil, "Idempotency-Key", "k1")
	b := m.want(201, "POST", "/api/v1/hosts/invites", alice, nil, "Idempotency-Key", "k1")
	if a["code"] != b["code"] {
		t.Fatalf("replay minted a new code: %v vs %v", a, b)
	}
	m.want(201, "POST", "/api/v1/members", alice, map[string]string{"email": "x@example.com"}, "Idempotency-Key", "k2")
	got := m.want(422, "POST", "/api/v1/members", alice, map[string]string{"email": "y@example.com"}, "Idempotency-Key", "k2")
	if got["code"] != "idempotency_mismatch" {
		t.Fatalf("mismatch = %v", got)
	}
}

func TestPairAttachDetach(t *testing.T) {
	f := New(t)
	alice := f.AddOwner("alice@example.com")
	sat1 := f.AddSatellite(t, "alice@example.com", "zoo")
	sat2 := f.AddSatellite(t, "alice@example.com", "lab")
	m := call{t, f.URL()}

	// Pair once — identity only.
	code := str(m.want(201, "POST", "/api/v1/hosts/invites", alice, nil), "code")
	pairBody := map[string]string{"code": code, "host": "studio", "os": "darwin", "arch": "arm64"}
	pr := m.want(201, "POST", "/api/v1/hosts/pair", "", pairBody)
	if pr["tunnel"] != false || str(obj(pr, "host"), "owner_email") != "alice@example.com" {
		t.Fatalf("pair = %v", pr)
	}
	host := str(pr, "token")
	if again := m.want(200, "POST", "/api/v1/hosts/pair", "", pairBody); str(again, "token") != host {
		t.Fatalf("pair retry = %v", again)
	}
	m.wantErr(409, "code_consumed", "POST", "/api/v1/hosts/pair", "", map[string]string{"code": code, "host": "other"})
	m.wantErr(401, "invalid_code", "POST", "/api/v1/hosts/pair", "", map[string]string{"code": "nope", "host": "x"})

	// Host Bearer satellite list.
	sats := list(m.want(200, "GET", "/api/v1/satellites", host, nil), "satellites")
	if len(sats) != 2 || sats[0].(map[string]any)["attached"] != false {
		t.Fatalf("host satellites = %v", sats)
	}
	// A host token is not an owner token.
	m.wantErr(403, "insufficient_scope", "GET", "/api/v1/members", host, nil)

	// Attach: master issues the attach token, the satellite redeems it.
	at := m.want(201, "POST", "/api/v1/satellites/"+sat1+"/attach", host, nil)
	endpoint := str(obj(at, "satellite"), "endpoint")
	s := call{t, endpoint}
	if as := list(m.want(200, "GET", "/api/v1/satellites/assignment", alice, nil), "assignments"); len(as) != 1 || str(as[0].(map[string]any), "state") != "pending" {
		t.Fatalf("assignment before redeem = %v", as)
	}
	s.wantErr(422, "host_mismatch", "POST", "/api/v1/hosts/attach", str(at, "attach_token"), map[string]string{"host": "intruder"})
	body := map[string]string{"host": "studio", "os": "darwin"}
	red := s.want(201, "POST", "/api/v1/hosts/attach", str(at, "attach_token"), body)
	satTok := str(red, "token")
	if str(red, "satellite_id") != sat1 || satTok == "" {
		t.Fatalf("redeem = %v", red)
	}
	if again := s.want(200, "POST", "/api/v1/hosts/attach", str(at, "attach_token"), body); str(again, "token") != satTok {
		t.Fatalf("redeem retry = %v", again)
	}
	s.wantErr(401, "attach_token_used", "POST", "/api/v1/hosts/attach", str(at, "attach_token"), map[string]string{"host": "studio", "os": "linux"})
	// Wrong audience: sat1's token at sat2.
	s2 := call{t, str(obj(m.want(200, "GET", "/api/v1/satellites/"+sat2, alice, nil), "satellite"), "endpoint")}
	s2.wantErr(401, "invalid_attach_token", "POST", "/api/v1/hosts/attach", str(at, "attach_token"), body)

	sh := obj(m.want(200, "GET", "/api/v1/satellites/"+sat1, alice, nil), "satellite")
	if hs := list(sh, "hosts"); len(hs) != 1 || hs[0] != "studio" {
		t.Fatalf("owner show = %v", sh)
	}
	if hv := obj(m.want(200, "GET", "/api/v1/satellites/"+sat1, host, nil), "satellite"); hv["attached"] != true || hv["hosts"] != nil {
		t.Fatalf("host show = %v", hv)
	}
	as := list(m.want(200, "GET", "/api/v1/satellites/assignment?satellite="+sat1, alice, nil), "assignments")
	if len(as) != 1 || str(as[0].(map[string]any), "state") != "attached" {
		t.Fatalf("assignment = %v", as)
	}

	got := m.want(409, "POST", "/api/v1/satellites/"+sat2+"/attach", host, nil)
	if got["code"] != "attached_elsewhere" || got["satellite_id"] != sat1 {
		t.Fatalf("attach elsewhere = %v", got)
	}

	// Detach via the master revokes the satellite host token.
	m.want(204, "DELETE", "/api/v1/satellites/"+sat1+"/attach", host, nil)
	m.want(204, "DELETE", "/api/v1/satellites/"+sat1+"/attach", host, nil)
	s.wantErr(401, "unauthorized", "DELETE", "/api/v1/hosts/self", satTok, nil)

	// Re-attach to sat2, then leave from the satellite side.
	f.SetSatelliteState(sat2, "stopped")
	m.wantErr(409, "satellite_unavailable", "POST", "/api/v1/satellites/"+sat2+"/attach", host, nil)
	f.SetSatelliteState(sat2, "enabled")
	at2 := m.want(201, "POST", "/api/v1/satellites/"+sat2+"/attach", host, nil)
	tok2 := str(s2.want(201, "POST", "/api/v1/hosts/attach", str(at2, "attach_token"), body), "token")
	s2.want(204, "DELETE", "/api/v1/hosts/self", tok2, nil)
	if as := list(m.want(200, "GET", "/api/v1/satellites/assignment", alice, nil), "assignments"); len(as) != 0 {
		t.Fatalf("assignment after leave = %v", as)
	}

	// Expired attach token.
	at3 := m.want(201, "POST", "/api/v1/satellites/"+sat2+"/attach", host, nil)
	later := time.Now().Add(AttachTokenTTL + time.Minute)
	f.SetNow(func() time.Time { return later })
	s2.wantErr(410, "attach_token_expired", "POST", "/api/v1/hosts/attach", str(at3, "attach_token"), body)
	f.SetNow(func() time.Time { return time.Now().UTC() })

	// Unpair: the host token is gone everywhere.
	m.want(204, "DELETE", "/api/v1/hosts/self", host, nil)
	m.wantErr(401, "unauthorized", "DELETE", "/api/v1/hosts/self", host, nil)
	if as := list(m.want(200, "GET", "/api/v1/satellites/assignment", alice, nil), "assignments"); len(as) != 0 {
		t.Fatalf("assignment after unpair = %v", as)
	}
}

func TestErrorEnvelopeOnUnknownRoute(t *testing.T) {
	f := New(t)
	m := call{t, f.URL()}
	m.wantErr(404, "not_found", "GET", "/api/v1/nope", "", nil)
	_, _, hdr := m.do("GET", "/api/v1/nope", "", nil)
	if !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", hdr)
	}
}
