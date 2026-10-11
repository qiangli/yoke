package webconsole

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/meet"
	"github.com/qiangli/yoke/pkg/websession"
)

func peerStore(t *testing.T) *pairStore {
	t.Helper()
	return newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
}

// An unknown sender's first message mints a code; a resend while it is open
// reports pending and never re-shows (or re-mints) a code.
func TestPeerRequestMintsOneCodeThenReportsPending(t *testing.T) {
	s := peerStore(t)
	first, err := s.requestPeer("slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.Code == "" || first.Pending || len(canonPeerCode(first.Code)) != 8 {
		t.Fatalf("first request = %+v", first)
	}
	again, err := s.requestPeer("slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Pending || again.Code != "" || again.Ticket != first.Ticket {
		t.Fatalf("resend = %+v, want the same pending ticket and no code", again)
	}
	st, _ := s.load()
	if len(st.Tickets) != 1 || st.Tickets[0].Hash == canonPeerCode(first.Code) {
		t.Fatalf("store must hold exactly one HASHED ticket: %+v", st.Tickets)
	}
	if _, ok, _ := s.peerDevice("slack", "alice"); ok {
		t.Fatal("a request alone must not pair the sender")
	}
}

// Approval redeems the code into a scoped, expiring device bound to the
// sender — and the code works exactly once.
func TestPeerApproveIsSingleUseAndScoped(t *testing.T) {
	s := peerStore(t)
	req, _ := s.requestPeer("slack", "alice")
	d, err := s.approvePeer(strings.ToLower(req.Code), []string{"approve", "ask"}, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if d.Peer == nil || *d.Peer != (peer{"slack", "alice"}) || strings.Join(d.Scope, ",") != "approve,ask" {
		t.Fatalf("device = %+v", d)
	}
	if got := d.Expires.Sub(d.Issued); got != 2*time.Hour {
		t.Fatalf("device lifetime = %s", got)
	}
	if _, err := s.approvePeer(req.Code, nil, 0); !errors.Is(err, errTicketUsed) {
		t.Fatalf("second approval err = %v, want single-use refusal", err)
	}
	got, ok, err := s.peerDevice("slack", "alice")
	if err != nil || !ok || got.ID != d.ID {
		t.Fatalf("peerDevice = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.peerDevice("slack", "bob"); ok {
		t.Fatal("a device is bound to exactly its own sender")
	}
	if _, ok, _ := s.peerDevice("discord", "alice"); ok {
		t.Fatal("a device is bound to exactly its own channel")
	}
}

func TestPeerScopeRejectsUnknownNames(t *testing.T) {
	s := peerStore(t)
	req, _ := s.requestPeer("slack", "alice")
	if _, err := s.approvePeer(req.Code, []string{"ask", "terminal"}, 0); err == nil {
		t.Fatal("an unknown scope must be refused, not dropped")
	}
	d, err := s.approvePeer(req.Code, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.Scope, ",") != meet.PeerScopeAsk {
		t.Fatalf("default scope = %v, want ask", d.Scope)
	}
}

// An unapproved code expires; a device expires.
func TestPeerCodeAndDeviceExpire(t *testing.T) {
	s := peerStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	req, _ := s.requestPeer("slack", "alice")
	s.now = func() time.Time { return now.Add(peerTicketWindow + time.Second) }
	if _, err := s.approvePeer(req.Code, nil, 0); !errors.Is(err, errTicketStale) {
		t.Fatalf("stale approval err = %v", err)
	}
	// The expired ticket no longer blocks: the next message mints a new code.
	fresh, err := s.requestPeer("slack", "alice")
	if err != nil || fresh.Code == "" || fresh.Code == req.Code {
		t.Fatalf("after expiry = %+v err=%v", fresh, err)
	}
	if _, err := s.approvePeer(fresh.Code, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(peerTicketWindow + 2*time.Minute) }
	if _, ok, _ := s.peerDevice("slack", "alice"); ok {
		t.Fatal("an expired device still admits the sender")
	}
}

// A denied code closes without a device, and resending does not mint another.
func TestPeerDenyIsRemembered(t *testing.T) {
	s := peerStore(t)
	req, _ := s.requestPeer("slack", "mallory")
	if _, err := s.denyPeer(req.Code); err != nil {
		t.Fatal(err)
	}
	again, err := s.requestPeer("slack", "mallory")
	if err != nil || !again.Denied || again.Code != "" {
		t.Fatalf("after deny = %+v err=%v", again, err)
	}
	if _, err := s.approvePeer(req.Code, nil, 0); !errors.Is(err, errPeerDenied) {
		t.Fatalf("approving a denied code err = %v", err)
	}
}

// Revoke ends the sender's device; revoke --all ends it with every other.
func TestPeerRevokeAndRevokeAll(t *testing.T) {
	s := peerStore(t)
	for _, who := range []string{"alice", "bob"} {
		req, _ := s.requestPeer("slack", who)
		if _, err := s.approvePeer(req.Code, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.revokePeer("slack", "alice")
	if err != nil || n != 1 {
		t.Fatalf("revokePeer = %d, %v", n, err)
	}
	if _, ok, _ := s.peerDevice("slack", "alice"); ok {
		t.Fatal("alice still paired after revoke")
	}
	if n, _ := s.revokePeer("slack", "alice"); n != 0 {
		t.Fatalf("second revoke = %d, want 0", n)
	}
	// Revocation re-opens pairing: the next message gets a new code.
	if req, _ := s.requestPeer("slack", "alice"); req.Code == "" {
		t.Fatalf("revoked sender gets no new code: %+v", req)
	}
	if _, err := s.revokeAll(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.peerDevice("slack", "bob"); ok {
		t.Fatal("revoke --all must end messenger devices too")
	}
}

// A messenger code is not a browser ticket, and a messenger device is not a
// browser session; neither holds the LAN listener open.
func TestPeerPairingNeverReachesTheBrowserSurface(t *testing.T) {
	h, store := pairEnv(t)
	req, err := store.requestPeer("slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(canonPeerCode(req.Code)), "192.168.1.44:5555", nil)
	if w.Code == http.StatusSeeOther {
		t.Fatal("a messenger code was redeemed as a browser pairing")
	}
	st, _ := store.load()
	if st.openTickets(time.Now()) != 0 {
		t.Fatal("a messenger code must not hold the LAN listener open")
	}

	d, err := store.approvePeer(req.Code, []string{"ask", "control", "approve"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, _ = store.load()
	if st.browserDevices(time.Now()) != 0 {
		t.Fatal("a messenger device must not hold the LAN listener open")
	}
	sessions := websession.NewStore(12*time.Hour, []byte("test-key-test-key-test-key-32byt"))
	cookie, err := sessions.Mint(deviceSubject(currentOSUser(), d.ID))
	if err != nil {
		t.Fatal(err)
	}
	// /api/session is console-wide — any admitted device reaches it whatever
	// its scope — so only the peer check can refuse it.
	if got := do(h, "GET", "/api/session", "192.168.1.44:5555", withCookie(&http.Cookie{Name: sessionCookie, Value: cookie})).Code; got != http.StatusForbidden {
		t.Fatalf("a session naming a messenger device = %d, want 403", got)
	}
}

// The CLI the operator types: approve, then deny of the same code is refused.
func TestPairApproveAndDenyCommands(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	req, err := peerPairing{}.Request("slack", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewAppsCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"pair", "approve", req.Code, "--allow", "ask,approve", "--ttl", "1h"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("approve: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "paired alice on channel slack") || !strings.Contains(out.String(), "approve, ask") {
		t.Fatalf("approve output = %q", out.String())
	}
	p, ok, err := peerPairing{}.Paired("slack", "alice")
	if err != nil || !ok || len(p.Scope) != 2 {
		t.Fatalf("Paired = %+v ok=%v err=%v", p, ok, err)
	}

	cmd = NewAppsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"pair", "deny", req.Code})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("deny of a used code err = %v", err)
	}
}
