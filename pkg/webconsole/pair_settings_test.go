package webconsole

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/websession"
)

// pairSettingsEnv builds a LAN-bound, pairing-armed console with a private
// store and a session store the test controls, so it can mint an operator
// cookie and inspect the tickets the Settings mint writes — without ever
// touching the developer's own pairing state.
func pairSettingsEnv(t *testing.T) (http.Handler, *pairStore, *websession.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	sessions := websession.NewStore(12*time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin:  true,
		Sessions:      sessions,
		Pairing:       true,
		PairStorePath: path,
	})
	return h, newPairStore(path), sessions
}

func operatorCookie(t *testing.T, sessions *websession.Store) *http.Cookie {
	t.Helper()
	v, err := sessions.Mint("someuser")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: v}
}

// stubPairAddresses pins the address probes so the dual-label assertions are
// deterministic regardless of the test host's interfaces or hostname.
func stubPairAddresses(t *testing.T, mdns, lan string) {
	t.Helper()
	om, ol := mdnsHostFn, lanAddrFn
	mdnsHostFn = func() string { return mdns }
	lanAddrFn = func() string { return lan }
	t.Cleanup(func() { mdnsHostFn, lanAddrFn = om, ol })
}

// 1. Default OFF: arming the console does not itself mint anything, and the
// session probe reports pairing so the page can render the right control
// without minting to find out.
func TestPairSettingsDefaultOffMintsNothing(t *testing.T) {
	h, store, sessions := pairSettingsEnv(t)
	c := operatorCookie(t, sessions)

	// Loading the page and the session probe must touch no ticket.
	do(h, "GET", "/api/session", "192.168.1.44:5555", withCookie(c))
	st, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if n := st.openTickets(time.Now()); n != 0 {
		t.Fatalf("open tickets after a plain load = %d, want 0 (default off must mint nothing)", n)
	}

	w := do(h, "GET", "/api/session", "192.168.1.44:5555", withCookie(c))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["pairing"] != true {
		t.Fatalf("session.pairing = %v, want true on a --pair console", body["pairing"])
	}
}

// 2. An authenticated operator mints a real ticket with two labelled,
// scan-ready codes; the payloads carry the versioned single-use ticket and no
// credential.
func TestPairMintOperatorSuccess(t *testing.T) {
	stubPairAddresses(t, "workshop.local", "192.168.1.20")
	h, store, sessions := pairSettingsEnv(t)
	c := operatorCookie(t, sessions)

	w := do(h, "POST", "/api/pair", "192.168.1.44:5555", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d, want 200 (body %q)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var resp struct {
		Enabled    bool     `json:"enabled"`
		Scope      []string `json:"scope"`
		PayloadVer string   `json:"payload_version"`
		Addresses  []struct {
			Kind      string `json:"kind"`
			Label     string `json:"label"`
			Host      string `json:"host"`
			AccessURL string `json:"access_url"`
			URL       string `json:"url"`
			QR        string `json:"qr"`
		} `json:"addresses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled {
		t.Fatal("mint did not report enabled")
	}
	// Default scope is the read-and-communicate set — not the terminal, not files.
	if got := strings.Join(resp.Scope, ","); got != "meet,mb,sprint" {
		t.Fatalf("scope = %q, want meet,mb,sprint", got)
	}

	// Exactly one ticket was minted, and it is open.
	st, _ := store.load()
	if n := st.openTickets(time.Now()); n != 1 {
		t.Fatalf("open tickets = %d, want 1", n)
	}

	// Two labelled addresses, distinct kinds, each a scannable QR whose payload
	// is the versioned redeem URL — never a bare address, never a credential.
	if len(resp.Addresses) != 2 {
		t.Fatalf("addresses = %d, want 2 (mdns + lan)", len(resp.Addresses))
	}
	kinds := map[string]bool{}
	for _, a := range resp.Addresses {
		kinds[a.Kind] = true
		if a.Label == "" {
			t.Fatalf("address %q has no label", a.Kind)
		}
		if !strings.HasPrefix(a.QR, "data:image/png;base64,") {
			t.Fatalf("address %q QR is not a PNG data URI: %.40q", a.Kind, a.QR)
		}
		wantAccess := "http://" + a.Host + ":22749/"
		if a.AccessURL != wantAccess {
			t.Fatalf("address %q access_url = %q, want %q", a.Kind, a.AccessURL, wantAccess)
		}
		u, err := url.Parse(a.URL)
		if err != nil {
			t.Fatalf("address %q URL unparseable: %v", a.Kind, err)
		}
		if u.Path != pairRedeemPath {
			t.Fatalf("address %q path = %q, want %q", a.Kind, u.Path, pairRedeemPath)
		}
		q := u.Query()
		if q.Get("v") != pairQRVersion {
			t.Fatalf("address %q missing version param", a.Kind)
		}
		if q.Get("t") == "" {
			t.Fatalf("address %q missing single-use ticket", a.Kind)
		}
		// The only query keys are the version and the ticket — no password, no
		// user, nothing but the single-use credential.
		for k := range q {
			if k != "v" && k != "t" {
				t.Fatalf("address %q carries unexpected query key %q", a.Kind, k)
			}
		}
		// The port must be the console's default (or an override), never a stale one.
		if !strings.Contains(u.Host, "22749") {
			t.Fatalf("address %q host = %q, want the Apps default port 22749", a.Kind, u.Host)
		}
	}
	if !kinds["mdns"] || !kinds["lan"] {
		t.Fatalf("kinds = %v, want both mdns and lan", kinds)
	}
}

// The Settings pairing flow also crosses a real HTTP server boundary: an
// authenticated POST mints a ticket and the returned redeem URL completes the
// one-time redirect flow. This catches routing/cookie behavior that direct
// handler calls cannot.
func TestPairSettingsE2EHTTPRoundTrip(t *testing.T) {
	stubPairAddresses(t, "workshop.local", "192.168.1.20")
	dir := t.TempDir()
	storePath := filepath.Join(dir, "pairing.json")
	sessions := websession.NewStore(12*time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h, closeHandler, err := Handler(Options{
		Ctx:           t.Context(),
		RequireLogin:  true,
		Sessions:      sessions,
		Pairing:       true,
		PairStorePath: storePath,
		Port:          22749,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cookie := operatorCookie(t, sessions)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/pair", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/pair = %d, want 200", resp.StatusCode)
	}
	var minted struct {
		Addresses []struct{ URL string } `json:"addresses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil || len(minted.Addresses) == 0 {
		t.Fatalf("decode mint response: %v", err)
	}
	u, err := url.Parse(minted.Addresses[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	client := *srv.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	redeem, err := client.Get(srv.URL + u.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	defer redeem.Body.Close()
	if redeem.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET redeem = %d, want 303", redeem.StatusCode)
	}
}

// 3. A console NOT armed for pairing fails closed: no ticket, and the exact
// restart command instead of a pretend "enabled".
func TestPairMintFailsClosedWhenNotArmed(t *testing.T) {
	stubPairAddresses(t, "workshop.local", "192.168.1.20")
	// A plain loopback dev console: no --pair, no requireLogin.
	h := newTestHandler(t, Options{})
	w := do(h, "POST", "/api/pair", "127.0.0.1:5555", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("mint on an unarmed console = %d, want 409", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["enabled"] != false {
		t.Fatalf("enabled = %v, want false", resp["enabled"])
	}
	restart, _ := resp["restart"].(string)
	if !strings.Contains(restart, "bashy app serve") || !strings.Contains(restart, "--pair") {
		t.Fatalf("restart = %q, want the exact `bashy app serve … --pair` command", restart)
	}
	// The guessed LAN address is filled in (stubbed), not left as a placeholder.
	if !strings.Contains(restart, "192.168.1.20") {
		t.Fatalf("restart = %q, want the derived LAN address", restart)
	}
}

// 4. A paired device cannot mint further tickets: the gate refuses /api/pair as
// outside every device scope.
func TestPairMintRejectsDeviceSession(t *testing.T) {
	h, store, _ := pairSettingsEnv(t)
	deviceCookie := redeemScan(t, h, store, nil, time.Hour)

	before, _ := store.load()
	w := do(h, "POST", "/api/pair", "192.168.1.44:5555", withCookie(deviceCookie))
	if w.Code != http.StatusForbidden {
		t.Fatalf("device mint = %d, want 403", w.Code)
	}
	after, _ := store.load()
	if len(after.Tickets) != len(before.Tickets) {
		t.Fatalf("a device minted a ticket: %d -> %d", len(before.Tickets), len(after.Tickets))
	}
}

// 5. An anonymous LAN visitor cannot mint: the gate refuses before the handler.
func TestPairMintRejectsAnonymousLAN(t *testing.T) {
	h, store, _ := pairSettingsEnv(t)
	w := do(h, "POST", "/api/pair", "192.168.1.44:5555", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("anonymous LAN mint = %d, want 403", w.Code)
	}
	st, _ := store.load()
	if len(st.Tickets) != 0 {
		t.Fatalf("an anonymous request minted %d ticket(s)", len(st.Tickets))
	}
}

// 6. Acceptance #4: an off-loopback, unpaired browser visit lands on the host-OS
// login page rather than being admitted.
func TestPairHostLoginEnforcedOffLoopback(t *testing.T) {
	h, _, _ := pairSettingsEnv(t)
	hdr := http.Header{}
	hdr.Set("Accept", "text/html")
	w := do(h, "GET", "/", "192.168.1.44:5555", hdr)
	if w.Code != http.StatusFound {
		t.Fatalf("unpaired LAN visit = %d, want 302 to /login", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/login") {
		t.Fatalf("redirect = %q, want the host-OS login page", loc)
	}
}

// 7. Lifecycle: a ticket minted from Settings redeems exactly once.
func TestPairSettingsLifecycle(t *testing.T) {
	stubPairAddresses(t, "workshop.local", "192.168.1.20")
	h, store, sessions := pairSettingsEnv(t)
	c := operatorCookie(t, sessions)

	w := do(h, "POST", "/api/pair", "192.168.1.44:5555", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d, want 200", w.Code)
	}
	var resp struct {
		Addresses []struct{ URL string } `json:"addresses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Addresses) == 0 {
		t.Fatalf("mint response unusable: %v", err)
	}
	u, _ := url.Parse(resp.Addresses[0].URL)
	redeem := pairRedeemPath + "?" + u.RawQuery

	first := do(h, "GET", redeem, "192.168.1.44:6666", nil)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first redemption = %d, want 303 (body %q)", first.Code, strings.TrimSpace(first.Body.String()))
	}
	if n := len(store.mustLoad(t).liveDevices(time.Now())); n != 1 {
		t.Fatalf("live devices after redeem = %d, want 1", n)
	}
	// Single use: the same ticket is refused the second time.
	second := do(h, "GET", redeem, "192.168.1.44:6666", nil)
	if second.Code == http.StatusSeeOther {
		t.Fatal("a Settings-minted ticket redeemed twice — not single-use")
	}
}

// 8. The Settings section is ALWAYS present and carries no on/off switch — it
// reads like Appearance or Background, with asking for a pass as an action the
// way Favorites has actions. A switch would imply a persistent setting, and a
// pairing pass is a single-use, time-boxed credential. It also always carries a
// collapsed, OS-selective Add-to-Home-Screen disclosure — content and
// accessibility (alt text) both present.
func TestPairSettingsRenderContent(t *testing.T) {
	h := newTestHandler(t, Options{}) // loopback dev console: / is ungated

	page := do(h, "GET", "/", "127.0.0.1:5555", nil).Body.String()
	if !strings.Contains(page, "Phone pairing") {
		t.Fatal("settings page is missing the Phone pairing section")
	}
	if !strings.Contains(page, `id="pair-btn"`) {
		t.Fatal("settings page is missing the pairing action button")
	}
	if !strings.Contains(page, `id="pair-instructions-host"`) {
		t.Fatal("settings page is missing the always-present install-help host")
	}
	// No on/off switch. Minting is an ACTION, not a setting: a switch suggests
	// a state that persists, while every pass is single-use and time-boxed.
	if strings.Contains(page, `id="pair-toggle"`) {
		t.Fatal("phone pairing still has an on/off switch; it must be an action button")
	}

	js := readArtifact(t, "app.js")
	for _, want := range []string{
		"Add to Home Screen", // iOS Safari step
		"Add to Home screen", // Android Chrome step
		"Safari", "Chrome",
		`document.createElement("details")`,
		"Show ${otherName} instructions",
		"Pairing QR code for ", // the QR <img alt> — accessibility
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js is missing instruction/accessibility content: %q", want)
		}
	}
}

// mustLoad is a load that fails the test rather than returning an error.
func (s *pairStore) mustLoad(t *testing.T) *pairState {
	t.Helper()
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func readArtifact(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(spaFS, name)
	if err != nil {
		t.Fatalf("read artifact %s: %v", name, err)
	}
	return string(b)
}
