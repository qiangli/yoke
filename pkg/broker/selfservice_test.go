package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/qiangli/yoke/pkg/coopauth"
)

const selfSecret = "test-sso-secret"

// selfPost sends POST /v1/peer/self as the outpost would: signed when email
// is non-empty and sign is true.
func selfPost(t *testing.T, h *harness, email string, sign bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, SelfServicePath, nil)
	prefix := "/matrix/h/door/app/llm"
	req.Header.Set(coopauth.HdrForwardedPrefix, prefix)
	if email != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set(coopauth.HdrRemoteUser, email)
		req.Header.Set(coopauth.HdrRemoteGroups, "admin")
		req.Header.Set(coopauth.HdrIdentityTs, ts)
		secret := selfSecret
		if !sign {
			secret = "forged"
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(email + "\nadmin\n" + prefix + "\n" + ts))
		req.Header.Set(coopauth.HdrIdentitySig, hex.EncodeToString(mac.Sum(nil)))
	}
	rec := httptest.NewRecorder()
	h.b.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestSelfServiceMintRefuseRotateRemove(t *testing.T) {
	clock := time.Now().UTC()
	alice := boundary.Member{Email: "alice@example.com", Circle: "team2027", Status: "active", Mutual: true}
	pending := boundary.Member{Email: "pat@example.com", Circle: "team2027", Status: "pending"}
	other := boundary.Member{Email: "olga@example.com", Circle: "family", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(alice, pending, other)
	server := httptest.NewServer(fixture)
	defer server.Close()
	client := &boundary.Client{BaseURL: server.URL, Token: "owner", HTTP: server.Client(), TTL: time.Second, Now: func() time.Time { return clock }}
	h, peers := memberPeerHarness(t, client)
	ssPath := SelfServiceFile(peers)

	// Off by default: nothing minted.
	if code, body := selfPost(t, h, alice.Email, true); code != http.StatusForbidden || errCode(body) != SelfCodeOff {
		t.Fatalf("default off = %d %v", code, body)
	}
	if err := SaveSelfService(ssPath, SelfService{Enabled: true, Circles: []string{"team2027"}, SSOSecret: selfSecret}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		email string
		sign  bool
		code  string
	}{
		{"", false, SelfCodeNotSigned},
		{alice.Email, false, SelfCodeNotSigned}, // forged signature
		{"nobody@example.com", true, SelfCodeNotMember},
		{pending.Email, true, SelfCodePending},
		{other.Email, true, SelfCodeWrongCirc},
	} {
		if _, body := selfPost(t, h, tc.email, tc.sign); errCode(body) != tc.code {
			t.Fatalf("%q sign=%v = %v, want %s", tc.email, tc.sign, body, tc.code)
		}
	}
	if ps, _ := LoadPeerShares(peers); len(ps.Peers) != 0 {
		t.Fatalf("refusals minted %v", ps.Names())
	}

	code, body := selfPost(t, h, alice.Email, true)
	if code != http.StatusOK || body["peer"] != "peer:self-alice@example.com" {
		t.Fatalf("mint = %d %v", code, body)
	}
	first := body["credential"].(string)
	if c, b := peerPost(t, h, "/v1/chat/completions", first, nil); c != http.StatusOK {
		t.Fatalf("chat with self key = %d %s", c, b)
	}
	recs := h.b.audit.snapshot()
	if got := recs[len(recs)-1].Principal; got != "peer:self-alice@example.com" {
		t.Fatalf("usage principal = %q", got)
	}

	// Rotation: old key 401, new key works, owner limits kept.
	if err := SetPeerLimits(peers, "self-alice@example.com", PeerLimits{RPM: 50}, clock); err != nil {
		t.Fatal(err)
	}
	_, body = selfPost(t, h, alice.Email, true)
	second := body["credential"].(string)
	if c, _ := peerPost(t, h, "/v1/chat/completions", first, nil); c != http.StatusUnauthorized {
		t.Fatalf("old key after rotation = %d", c)
	}
	if c, b := peerPost(t, h, "/v1/chat/completions", second, nil); c != http.StatusOK {
		t.Fatalf("new key = %d %s", c, b)
	}
	if ps, _ := LoadPeerShares(peers); ps.Peers["self-alice@example.com"].Limits == nil {
		t.Fatal("rotation dropped owner limits")
	}

	// An owner deny survives a rotation.
	if err := SetPeerShare(peers, "self-alice@example.com", ShareDeny, clock); err != nil {
		t.Fatal(err)
	}
	selfPost(t, h, alice.Email, true)
	if ps, _ := LoadPeerShares(peers); ps.Peers["self-alice@example.com"].Share != ShareDeny {
		t.Fatal("rotation overrode the owner's deny")
	}
	if err := SetPeerShare(peers, "self-alice@example.com", ShareAllow, clock); err != nil {
		t.Fatal(err)
	}
	_, body = selfPost(t, h, alice.Email, true)
	third := body["credential"].(string)

	// Off: no new mints, the existing key keeps working.
	if err := SaveSelfService(ssPath, SelfService{Enabled: false, Circles: []string{"team2027"}, SSOSecret: selfSecret}); err != nil {
		t.Fatal(err)
	}
	if _, body := selfPost(t, h, alice.Email, true); errCode(body) != SelfCodeOff {
		t.Fatalf("off mint = %v", body)
	}
	if c, b := peerPost(t, h, "/v1/chat/completions", third, nil); c != http.StatusOK {
		t.Fatalf("existing key while off = %d %s", c, b)
	}

	// Removal from the circle blocks the next request.
	fixture.set(pending, other)
	clock = clock.Add(2 * time.Second)
	if c, b := peerPost(t, h, "/v1/chat/completions", third, nil); c != http.StatusForbidden || !strings.Contains(b, "not an active mutual member") {
		t.Fatalf("removed member = %d %s", c, b)
	}
}

func TestSelfServiceRefusesOfflineMint(t *testing.T) {
	clock := time.Now().UTC()
	alice := boundary.Member{Email: "alice@example.com", Circle: "team2027", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(alice)
	server := httptest.NewServer(fixture)
	client := &boundary.Client{BaseURL: server.URL, Token: "owner", HTTP: server.Client(), TTL: time.Second, Now: func() time.Time { return clock }}
	h, peers := memberPeerHarness(t, client)
	if err := SaveSelfService(SelfServiceFile(peers), SelfService{Enabled: true, Circles: []string{"team2027"}, SSOSecret: selfSecret}); err != nil {
		t.Fatal(err)
	}
	if code, _ := selfPost(t, h, alice.Email, true); code != http.StatusOK {
		t.Fatalf("warm mint = %d", code)
	}
	server.Close()
	clock = clock.Add(time.Minute)
	if _, body := selfPost(t, h, alice.Email, true); errCode(body) != SelfCodeUnverified {
		t.Fatalf("offline mint = %v", body)
	}
}

func TestPeerStoreConcurrentUpdatesKeepRecords(t *testing.T) {
	peers := t.TempDir() + "/cligw/peers.json"
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := MintPeerCredentialFor(peers, "self-u"+strconv.Itoa(i)+"@example.com", ShareAllow, "u"+strconv.Itoa(i)+"@example.com", "c", now); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	ps, err := LoadPeerShares(peers)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Peers) != 20 {
		t.Fatalf("concurrent mints kept %d of 20 records", len(ps.Peers))
	}
}

func TestSelfPeerName(t *testing.T) {
	if n, err := SelfPeerName(" Alice@Example.COM "); err != nil || n != "self-alice@example.com" || !validSelfPeerName(n) {
		t.Fatalf("SelfPeerName = %q %v", n, err)
	}
	for _, bad := range []string{"", "no-at", "a@b c", "a/b@x.com", strings.Repeat("a", 250) + "@x.com"} {
		if _, err := SelfPeerName(bad); err == nil {
			t.Fatalf("SelfPeerName(%q) accepted", bad)
		}
	}
	if validSelfPeerName("self-Alice@example.com") || validSelfPeerName("alice@example.com") {
		t.Fatal("validSelfPeerName accepted a non-canonical name")
	}
}
