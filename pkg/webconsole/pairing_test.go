package webconsole

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/websession"
)

// pairEnv builds a console with pairing on and a private store, so a test
// never touches the developer's own pairing state.
func pairEnv(t *testing.T) (http.Handler, *pairStore) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	store := newPairStore(path)
	h := newTestHandler(t, Options{
		RequireLogin:  true,
		Sessions:      websession.NewStore(12*time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing:       true,
		PairStorePath: path,
	})
	return h, store
}

// redeem drives a scan: mint a ticket, GET the redeem URL, return the session
// cookie the console handed back.
func redeemScan(t *testing.T, h http.Handler, store *pairStore, scope []string, ttl time.Duration) *http.Cookie {
	t.Helper()
	_, secret, err := store.issueTicket(scope, ttl, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), "192.168.1.44:5555", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redeem = %d, want 303 (body %q)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("redeem set no session cookie")
	return nil
}

func withCookie(c *http.Cookie) http.Header {
	h := http.Header{}
	h.Set("Cookie", c.Name+"="+c.Value)
	return h
}

// TestScanOneQRReachesTheConsoleWithoutAPassword is the story's first
// acceptance line.
func TestScanOneQRReachesTheConsoleWithoutAPassword(t *testing.T) {
	h, store := pairEnv(t)

	// Before pairing, the LAN peer is refused.
	if got := do(h, "GET", "/sprint/", "192.168.1.44:5555", nil).Code; got == http.StatusOK {
		t.Fatal("an unpaired LAN peer reached the console")
	}
	cookie := redeemScan(t, h, store, nil, time.Hour)
	if got := do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusOK {
		t.Fatalf("paired device on /sprint/ = %d, want 200", got)
	}
}

func TestPairedDeviceCanLoadSharedChromeAssets(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, nil, time.Hour)
	for _, asset := range []string{
		"/app.css", "/backgrounds.css", "/app.js", "/board.js", "/mb.js",
		"/vendor/xterm.css", "/vendor/xterm.js", "/vendor/xterm-addon-fit.js",
	} {
		w := do(h, "GET", asset, "192.168.1.44:5555", withCookie(cookie))
		if w.Code != http.StatusOK {
			t.Errorf("paired device GET %s = %d, want 200 (body %q)", asset, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// TestSecondScanOfTheSameTicketFails: single-use in both directions.
func TestSecondScanOfTheSameTicketFails(t *testing.T) {
	h, store := pairEnv(t)
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	target := pairRedeemPath + "?v=" + pairQRVersion + "&t=" + url.QueryEscape(secret)
	if got := do(h, "GET", target, "192.168.1.44:5555", nil).Code; got != http.StatusSeeOther {
		t.Fatalf("first scan = %d, want 303", got)
	}
	w := do(h, "GET", target, "192.168.1.45:5555", nil)
	if w.Code == http.StatusSeeOther {
		t.Fatal("a pairing code was redeemed twice")
	}
	if !strings.Contains(w.Body.String(), "already been used") {
		t.Fatalf("the second scan does not say why it failed: %q", w.Body.String())
	}
}

// TestRevokingTheOperatorGrantRevokesDerivedDevices is the story's third
// acceptance line: a device inherits the operator authority and dies with it.
func TestRevokingTheOperatorGrantRevokesDerivedDevices(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, nil, time.Hour)
	if got := do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusOK {
		t.Fatalf("paired device = %d, want 200", got)
	}
	if _, err := store.revokeAll(); err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(cookie))
	if w.Code != http.StatusForbidden {
		t.Fatalf("after revoke --all = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "pairing has ended") {
		t.Fatalf("revoked device is refused without saying why: %q", w.Body.String())
	}
	// The cookie must be cleared, or the phone keeps presenting a dead one.
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("a dead device's cookie was not cleared")
	}
}

// TestRevokingOneDeviceLeavesTheOthers.
func TestRevokingOneDeviceLeavesTheOthers(t *testing.T) {
	h, store := pairEnv(t)
	a := redeemScan(t, h, store, nil, time.Hour)
	b := redeemScan(t, h, store, nil, time.Hour)

	st, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	live := st.liveDevices(time.Now())
	if len(live) != 2 {
		t.Fatalf("expected 2 live devices, got %d", len(live))
	}
	ok, err := store.revoke(live[0].ID)
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	// One of the two cookies still works, the other does not.
	codes := []int{
		do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(a)).Code,
		do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(b)).Code,
	}
	if !(codes[0] == http.StatusForbidden && codes[1] == http.StatusOK) &&
		!(codes[1] == http.StatusForbidden && codes[0] == http.StatusOK) {
		t.Fatalf("revoking one device did not leave exactly the other live: %v", codes)
	}
}

// TestExpiredDeviceIsRefused.
func TestExpiredDeviceIsRefused(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, nil, 50*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	if got := do(h, "GET", "/sprint/", "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusForbidden {
		t.Fatalf("expired device = %d, want 403", got)
	}
}

// TestDefaultScopeIsNotAShell is the per-panel-scope story's headline: a
// paired phone reaches sprint/mb/meet and NOT /term/ or /files/.
func TestDefaultScopeIsNotAShell(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, nil, time.Hour)

	allowed := []string{"/sprint/", "/mb/", "/meet/"}
	for _, p := range allowed {
		if got := do(h, "GET", p, "192.168.1.44:5555", withCookie(cookie)).Code; got == http.StatusForbidden {
			t.Fatalf("default scope refuses %s, which it is supposed to allow", p)
		}
	}
	for _, p := range []string{"/term/", "/files/"} {
		w := do(h, "GET", p, "192.168.1.44:5555", withCookie(cookie))
		if w.Code != http.StatusForbidden {
			t.Fatalf("default scope reaches %s (%d) — a scanned phone must not be a shell", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), "--allow") {
			t.Fatalf("scope denial on %s names no remedy: %q", p, w.Body.String())
		}
	}
}

// Stored grants are authority, not a migration surface. A device carrying a
// retired scope spelling fails closed instead of inheriting the renamed app.
func TestRetiredStoredScopesDoNotGrantCanonicalPanels(t *testing.T) {
	for retired, canonicalPath := range map[string]string{
		"board": "/sprint/",
		"relay": "/meet/",
	} {
		t.Run(retired, func(t *testing.T) {
			h, store := pairEnv(t)
			cookie := redeemScan(t, h, store, []string{retired}, time.Hour)
			if got := do(h, "GET", canonicalPath, "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusForbidden {
				t.Fatalf("stored %q scope reached %s with status %d", retired, canonicalPath, got)
			}
		})
	}
}

// TestAllowGrantsExactlyWhatItNames.
func TestAllowGrantsExactlyWhatItNames(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, []string{"terminal"}, time.Hour)
	if got := do(h, "GET", "/term/", "192.168.1.44:5555", withCookie(cookie)).Code; got == http.StatusForbidden {
		t.Fatal("--allow terminal did not grant the terminal")
	}
	if got := do(h, "GET", "/files/", "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusForbidden {
		t.Fatalf("--allow terminal also granted files (%d) — scope must grant exactly what it names", got)
	}
}

// TestScopeHoldsOnTheAPISurfaceToo is the acceptance line that makes the
// first-segment property real: an out-of-scope panel's DATA must be as
// unreachable as its page. /api/sprint would otherwise resolve to segment
// "api", which owns nothing, and sail straight through.
func TestScopeHoldsOnTheAPISurfaceToo(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, []string{"mb"}, time.Hour)

	if got := do(h, "GET", "/api/mb", "192.168.1.44:5555", withCookie(cookie)).Code; got == http.StatusForbidden {
		t.Fatal("an in-scope panel's API is refused")
	}
	if got := do(h, "GET", "/api/sprint", "192.168.1.44:5555", withCookie(cookie)).Code; got != http.StatusForbidden {
		t.Fatalf("/api/sprint reachable (%d) by a device scoped to mb only", got)
	}
	// The console-wide surfaces must stay reachable or the launcher cannot render.
	for _, p := range []string{"/", "/api/apps", "/api/session"} {
		if got := do(h, "GET", p, "192.168.1.44:5555", withCookie(cookie)).Code; got == http.StatusForbidden {
			t.Fatalf("console-wide path %s refused to a scoped device", p)
		}
	}
}

// TestScopeSegmentResolution pins the mapping directly, including the /api
// deferral and the name-vs-mount difference (terminal lives at /term/).
func TestScopeSegmentResolution(t *testing.T) {
	cases := map[string]string{
		"/sprint/":            "sprint",
		"/api/sprint":         "sprint",
		"/api/sprint/panel/7": "sprint",
		"/term/ws":            "term",
		"/files/x/y":          "files",
		"/":                   "",
	}
	for path, want := range cases {
		if got := scopeSegment(path); got != want {
			t.Fatalf("scopeSegment(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestUnknownPanelInAllowIsRejected: silently accepting `--allow term` would
// leave the operator believing they granted something they did not.
func TestUnknownPanelInAllowIsRejected(t *testing.T) {
	panels := Discover()
	if err := ValidateScope([]string{"sprint"}, panels); err != nil {
		t.Fatalf("a real panel was rejected: %v", err)
	}
	for _, retired := range []string{"board", "relay"} {
		if err := ValidateScope([]string{retired}, panels); err == nil {
			t.Errorf("retired panel name %q was accepted", retired)
		}
	}
	err := ValidateScope([]string{"nosuchpanel"}, panels)
	if err == nil {
		t.Fatal("an unknown panel name was accepted")
	}
	if !strings.Contains(err.Error(), "this console serves") {
		t.Fatalf("the error names no valid set: %v", err)
	}
	// The mount segment is accepted too, since that is what the gate sees.
	if err := ValidateScope([]string{"term"}, panels); err != nil {
		t.Fatalf("the mount segment was rejected: %v", err)
	}
}

// A DEVICE NEVER OUTLIVES ITS GRANT — and an explicit request EXTENDS the grant
// rather than being quietly shortened to fit it.
//
// This replaces an assertion that the grant ceiling always wins. That rule read
// as safety and behaved as a silent failure: an operator who asked for a day
// got a day back in the response and a phone that stopped working after twelve
// hours, with nothing anywhere connecting the two. The property worth keeping
// is the RELATIONSHIP (a device session hangs off a grant and cannot outlast
// it), not the ceiling's fixed value — the operator is allowed to say how long
// their own authority runs, and revocation, not expiry, is the control that
// ends a device early.
func TestAnExplicitDeviceTTLExtendsTheGrantRatherThanBeingClamped(t *testing.T) {
	store := newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
	asked := 365 * 24 * time.Hour
	tk, _, err := store.issueTicket(nil, asked, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := time.ParseDuration(tk.TTL)
	if err != nil {
		t.Fatal(err)
	}
	if ttl < asked {
		t.Fatalf("device TTL %s is shorter than the %s that was asked for", ttl, asked)
	}

	st, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Grant == nil {
		t.Fatal("a ticket was issued with no grant behind it")
	}
	// The device deadline is TTL from issue — the ticket's own Expires is the
	// SCAN window, a different and much shorter clock. Asserting on that one
	// instead is how this test could look right and check nothing.
	deviceExpires := tk.Issued.Add(ttl)
	if deviceExpires.After(st.Grant.Expires) {
		t.Errorf("device would expire %s, after its grant %s — a device must not outlive the authority it hangs off",
			deviceExpires, st.Grant.Expires)
	}
}

// The DEFAULT is a day, and it is the value applied when no TTL is asked for.
//
// Asserted separately from the extension above because the two answer different
// questions: how long an unattended pairing lasts, versus whether an explicit
// request is honoured. The default is the one every operator gets without
// choosing.
func TestTheDefaultDeviceTTLIsADay(t *testing.T) {
	store := newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
	tk, _, err := store.issueTicket(nil, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := time.ParseDuration(tk.TTL)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != defaultDeviceTTL {
		t.Fatalf("default device TTL = %s, want %s", ttl, defaultDeviceTTL)
	}
	if defaultDeviceTTL != 24*time.Hour {
		t.Errorf("the default is %s; the documented default is 24h", defaultDeviceTTL)
	}
}

// "Never expires" is a DATE, not a sentinel.
//
// Every reader of a device record compares against a time — the gate, the CLI
// listing, the Settings table — so a zero or a null would have to be understood
// by all of them, and the one that forgot would either lock the phone out or
// keep a genuinely expired device alive. A century out needs no special case
// anywhere, and is still recognisably "never" to the one place that says so in
// words.
func TestNeverExpiresIsStoredAsADateFarEnoughOut(t *testing.T) {
	store := newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
	tk, _, err := store.issueTicket(nil, neverExpiresTTL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := time.ParseDuration(tk.TTL)
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= neverExpiresAfter {
		t.Fatalf("a never-expiring device lasts %s, inside the %s window that still reads as a date",
			ttl, neverExpiresAfter)
	}
	// And the ticket itself is still SHORT-LIVED. The two clocks are
	// independent, and confusing them would turn "the phone stays paired" into
	// "the code on this screen is good for a century".
	if window := tk.Expires.Sub(tk.Issued); window > time.Hour {
		t.Errorf("the scan window grew to %s along with the device TTL", window)
	}
}

// The mint endpoint's own reading of the operator's choice, where ZERO IS THE
// SPELLING FOR NEVER — the opposite of the store's convention one layer down,
// which is exactly why this conversion exists at the boundary rather than being
// left for the store to guess.
func TestTheMintEndpointReadsZeroHoursAsNever(t *testing.T) {
	hours := func(h float64) *float64 { return &h }
	if got := deviceTTLFrom(nil); got != 0 {
		t.Errorf("an absent choice = %s, want the store's default (0)", got)
	}
	if got := deviceTTLFrom(hours(-1)); got != 0 {
		t.Errorf("a negative choice = %s, want the store's default (0)", got)
	}
	if got := deviceTTLFrom(hours(0)); got != neverExpiresTTL {
		t.Errorf("zero hours = %s, want never (%s)", got, neverExpiresTTL)
	}
	if got := deviceTTLFrom(hours(24)); got != 24*time.Hour {
		t.Errorf("24 hours = %s, want 24h", got)
	}
}

// TestTicketSecretIsNeverWrittenToDisk. Reading the pairing file must not let
// an attacker perform a pairing.
func TestTicketSecretIsNeverWrittenToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairing.json")
	store := newPairStore(path)
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the pairing file contains the ticket secret in the clear")
	}
	if !strings.Contains(string(raw), hashSecret(secret)) {
		t.Fatal("the pairing file does not contain the ticket hash — redemption cannot work")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("pairing file mode = %o, want 600", perm)
	}
}

// TestQRPayloadIsVersioned: a v2 payload (one carrying a certificate
// fingerprint) must be REFUSED by a v1 console rather than half-understood.
func TestQRPayloadIsVersioned(t *testing.T) {
	h, store := pairEnv(t)
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", pairRedeemPath+"?v=2&t="+url.QueryEscape(secret), "192.168.1.44:5555", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a v2 payload was accepted by a v1 console (%d)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "newer bashy") {
		t.Fatalf("version refusal names no remedy: %q", w.Body.String())
	}
	// The ticket must still be unspent after a version refusal.
	st, _ := store.load()
	if st.openTickets(time.Now()) != 1 {
		t.Fatal("a refused version consumed the ticket")
	}
}

// TestRedeemURLCarriesTheVersion.
func TestRedeemURLCarriesTheVersion(t *testing.T) {
	got := redeemURL("workshop.local", 8639, "sec-ret")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != pairRedeemPath {
		t.Fatalf("path = %q", u.Path)
	}
	if u.Query().Get("v") != pairQRVersion || u.Query().Get("t") != "sec-ret" {
		t.Fatalf("payload = %q", got)
	}
	if u.Host != "workshop.local:8639" {
		t.Fatalf("host = %q", u.Host)
	}
}

// TestPairingIsOffByDefault: no --pair means no redemption route at all.
func TestPairingIsOffByDefault(t *testing.T) {
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Sessions:     websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
	})
	w := do(h, "GET", pairRedeemPath+"?t=anything", "192.168.1.44:5555", nil)
	if w.Code == http.StatusSeeOther {
		t.Fatal("a console without --pair redeemed a pairing ticket")
	}
}

// TestDevicesJSONIsTypedData, not a rendered table.
func TestDevicesJSONIsTypedData(t *testing.T) {
	h, store := pairEnv(t)
	redeemScan(t, h, store, []string{"sprint"}, time.Hour)
	st, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	live := st.liveDevices(time.Now())
	if len(live) != 1 {
		t.Fatalf("want 1 device, got %d", len(live))
	}
	raw, err := json.Marshal(live[0])
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	scope, ok := back["scope"].([]any)
	if !ok || len(scope) != 1 || scope[0] != "sprint" {
		t.Fatalf("scope is not typed data: %#v", back["scope"])
	}
}

// TestTerminalQRRenders: the code has to be scannable, and a QR that fails to
// render must say so rather than print an empty block.
func TestTerminalQRRenders(t *testing.T) {
	block, err := terminalQR(redeemURL("192.168.1.20", 8639, strings.Repeat("a", 43)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if len(lines) < 12 {
		t.Fatalf("QR block is only %d lines; that is not a scannable code", len(lines))
	}
	// Square-ish: a QR that renders as one long line is a rendering bug.
	if w := len([]rune(lines[len(lines)/2])); w < 20 {
		t.Fatalf("QR block is %d columns wide", w)
	}
}

// TestPairAuditRecordsTheGrant: a grant nobody can audit is a grant nobody can
// supervise. These records are NOT gated on $BASHY_AUDIT.
func TestPairAuditRecordsTheGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_AUDIT", "") // explicitly off for commands
	auditPairEvent("pair.issued", map[string]string{"ticket": "abc", "scope": "sprint"})

	logPath := filepath.Join(home, "audit", "audit.jsonl")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("pairing left no audit record: %v", err)
	}
	if !strings.Contains(string(raw), "pair.issued") || !strings.Contains(string(raw), "ticket=abc") {
		t.Fatalf("audit record does not describe the pairing: %s", raw)
	}
}

// TestPairGatedListenerFollowsTheDeviceSet is the structural claim: exposure
// is a FUNCTION of the paired device set, not a switch left on.
func TestPairGatedListenerFollowsTheDeviceSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	store := newPairStore(path)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	var out strings.Builder
	stop, err := runPairGatedListener(ctx, &out, srv, addr, path)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Nothing paired: the port must NOT be open.
	time.Sleep(3 * time.Second)
	if dialable(addr) {
		t.Fatalf("the LAN port is open with nothing paired:\n%s", out.String())
	}

	// An outstanding ticket opens it (somebody is about to scan).
	if _, _, err := store.issueTicket(nil, time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !eventually(6*time.Second, func() bool { return dialable(addr) }) {
		t.Fatalf("the LAN port did not open for a pending pairing:\n%s", out.String())
	}

	// Revoking everything closes it again, with no operator action.
	if _, err := store.revokeAll(); err != nil {
		t.Fatal(err)
	}
	if !eventually(8*time.Second, func() bool { return !dialable(addr) }) {
		t.Fatalf("the LAN port stayed open after the last pairing ended:\n%s", out.String())
	}
}

// A console daemon outlives DHCP leases and VPN sessions. When its configured
// IP disappears, the gated phone listener must move to the newly selected LAN
// address instead of retrying the stale one forever.
func TestPairGatedListenerFollowsANetworkChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	store := newPairStore(path)
	if _, _, err := store.issueTicket(nil, time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}

	reserve := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}
	first, second := reserve(), reserve()
	for second == first {
		second = reserve()
	}
	var current atomic.Value
	current.Store(first)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	stop, err := runPairGatedListenerWithAddr(ctx, io.Discard, srv, first, path, func(string) string {
		return current.Load().(string)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if !eventually(6*time.Second, func() bool { return dialable(first) }) {
		t.Fatal("the initial LAN listener did not open")
	}
	current.Store(second)
	if !eventually(6*time.Second, func() bool { return dialable(second) && !dialable(first) }) {
		t.Fatal("the LAN listener did not follow the network change")
	}
}

func TestCurrentPairListenerAddrReplacesAStaleIP(t *testing.T) {
	current := primaryLANAddr()
	if current == "" {
		t.Skip("host has no LAN address")
	}
	if got, want := currentPairListenerAddr("192.0.2.1:22749"), net.JoinHostPort(current, "22749"); got != want {
		t.Fatalf("stale pairing address resolved to %q, want %q", got, want)
	}
}

func dialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// TestOperatorSessionIsUnaffectedByScope: an ordinary host-OS login keeps the
// full panel set. Narrowing the phone must not narrow the laptop.
func TestOperatorSessionIsUnaffectedByScope(t *testing.T) {
	dir := t.TempDir()
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin:  true,
		Sessions:      sessions,
		Pairing:       true,
		PairStorePath: filepath.Join(dir, "pairing.json"),
	})
	cookieVal, err := sessions.Mint("someuser")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Cookie{Name: sessionCookie, Value: cookieVal}
	for _, p := range []string{"/", "/sprint/", "/files/", "/term/"} {
		if got := do(h, "GET", p, "192.168.1.44:5555", withCookie(c)).Code; got == http.StatusForbidden {
			t.Fatalf("an operator session was refused %s", p)
		}
	}
}

// TestSplitDeviceSubject pins the cookie subject encoding, including the fact
// that an ordinary operator subject is NOT read as a device.
func TestSplitDeviceSubject(t *testing.T) {
	u, d, ok := splitDeviceSubject(deviceSubject("alice", "dev123"))
	if !ok || u != "alice" || d != "dev123" {
		t.Fatalf("round trip = %q %q %v", u, d, ok)
	}
	if _, _, ok := splitDeviceSubject("alice"); ok {
		t.Fatal("a bare operator subject was read as a device")
	}
	if _, err := websession.NewStore(time.Hour, nil).Mint(deviceSubject("alice", "dev123")); err != nil {
		t.Fatalf("the device subject is not mintable: %v", err)
	}
}

var _ = httptest.NewRequest

// TestUnreadableTTLIsRefusedNotDefaulted pins the direction this must fail in.
// An earlier draft rounded the stored TTL to the second, so a sub-second TTL
// became "0s", which redeem read as unparseable and replaced with the 4-hour
// default — silently WIDENING a grant the operator had narrowed. A ticket this
// code cannot read is a refusal.
func TestUnreadableTTLIsRefusedNotDefaulted(t *testing.T) {
	store := newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.mutate(func(st *pairState) error {
		st.Tickets[0].TTL = "not-a-duration"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.redeem(secret, "phone", ""); err == nil {
		t.Fatal("a ticket with an unreadable lifetime was redeemed anyway")
	}

	// And a sub-second TTL survives the round trip as itself.
	store2 := newPairStore(filepath.Join(t.TempDir(), "pairing.json"))
	_, secret2, err := store2.issueTicket(nil, 200*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store2.redeem(secret2, "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(d.Expires); got > time.Second {
		t.Fatalf("a 200ms TTL became %s — the grant was widened", got)
	}
}

// TestScopeDenialNamesThePanelTheOperatorTypes: the gate sees the MOUNT
// segment ("term") while --allow takes the panel NAME ("terminal"). Echoing
// the segment back would hand the reader a remedy they have to translate.
func TestScopeDenialNamesThePanelTheOperatorTypes(t *testing.T) {
	h, store := pairEnv(t)
	cookie := redeemScan(t, h, store, []string{"sprint"}, time.Hour)
	body := do(h, "GET", "/term/", "192.168.1.44:5555", withCookie(cookie)).Body.String()
	if !strings.Contains(body, "--allow terminal") {
		t.Fatalf("denial suggests a name --allow does not take: %q", body)
	}
}

// TestAuditRecordsAreTimestamped. audit.Append fills Seq/PrevHash/Hash but not
// Time; a pairing record whose timestamp is empty cannot answer "when was this
// device let in?", which is most of the point of recording it.
func TestAuditRecordsAreTimestamped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	auditPairEvent("pair.issued", map[string]string{"ticket": "t1"})
	raw, err := os.ReadFile(filepath.Join(home, "audit", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Time string `json:"time"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Time == "" {
		t.Fatal("pairing audit record carries no timestamp")
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.Time); err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", rec.Time, err)
	}
}

// A freshly paired phone lands on the LAUNCHER, not on whichever panel happens
// to be first in its scope. Dropping a device straight into /sprint/ gives it no
// sense of where it is or what else it can reach; "/" is the console's one nav,
// it lists exactly the panels this device is scoped to, and it is a
// consoleWidePath, so the landing can never be a page the device is refused.
func TestPairedDeviceLandsOnTheAppsLauncher(t *testing.T) {
	h, store := pairEnv(t)

	_, secret, err := store.issueTicket(nil, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret),
		"192.168.1.44:5555", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redeem = %d, want 303", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("paired device lands on %q, want %q (the apps launcher)", got, "/")
	}
}
