// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/websession"
)

// backslashOwner is the synthetic stand-in for the qualified account name Go
// reports on native Windows (`DOMAIN\user`). A backslash is not a valid
// cookie-value byte, so a session subject carrying one used to be mangled by
// net/http parsing — dropping bytes, breaking the MAC, and 403ing every
// paired-device request.
const backslashOwner = `TESTDOM\testuser`

// injectBackslashOwner resolves the console's owner the way the code does —
// through osUserLookup, behind currentOSUser — with a qualified name no Unix
// test host will ever have.
func injectBackslashOwner(t *testing.T) {
	t.Helper()
	prev := osUserLookup
	osUserLookup = func() string { return backslashOwner }
	t.Cleanup(func() { osUserLookup = prev })
}

// backslashConsole builds a login-gated console with pairing on and a notes
// panel whose upstream echoes the principal the gate resolved, so the test
// asserts on identity rather than on status codes alone.
func backslashConsole(t *testing.T) (http.Handler, *pairStore, *websession.Store) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get(PrincipalHeader)))
	}))
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	pairPath := filepath.Join(t.TempDir(), "pairing.json")
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Auth:         okAuth{password: "correct-horse"},
		Sessions:     sessions,
		Pairing:      true, PairStorePath: pairPath,
		Panels: []Panel{{Name: "notes", Label: "Notes", Path: "/notes/", Mode: atlas.WebProxy,
			Port: port, Auth: AuthSystem, Source: "app", Available: true}},
	})
	return h, newPairStore(pairPath), sessions
}

// sessionCookieFrom extracts the session cookie through REAL net/http
// parsing — Response.Cookies on the way out, AddCookie/Cookie on the way
// back — and fails if the value does not survive byte-identical. A cookie
// carrying a raw backslash cannot pass: the parser drops the invalid byte.
func sessionCookieFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	res := w.Result()
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name != sessionCookie || c.Value == "" {
			continue
		}
		if strings.ContainsRune(c.Value, '\\') {
			t.Fatalf("cookie value carries a raw backslash: %q", c.Value)
		}
		back := httptest.NewRequest(http.MethodGet, "/notes/", nil)
		back.AddCookie(c)
		got, err := back.Cookie(sessionCookie)
		if err != nil || got.Value != c.Value {
			t.Fatalf("cookie did not survive a net/http round trip: %q", c.Value)
		}
		return c
	}
	t.Fatal("no session cookie was set")
	return nil
}

// principalOf asks the console who a cookie identifies as.
func principalOf(t *testing.T, h http.Handler, cookie *http.Cookie) (string, int) {
	t.Helper()
	w := do(h, http.MethodGet, "/notes/", "192.0.2.10:5555", withCookie(cookie))
	return strings.TrimSpace(w.Body.String()), w.Code
}

// tamperedValue flips one trailing character to another valid cookie byte,
// so the cookie still parses but the MAC no longer matches.
func tamperedValue(v string) string {
	if strings.HasSuffix(v, "A") {
		return v[:len(v)-1] + "B"
	}
	return v[:len(v)-1] + "A"
}

// oldFormatCookie builds a cookie in the pre-fix format the server used
// before subjects were encoded: `subject|exp|sig` with the MAC over the raw
// subject, keyed exactly as the old code keyed it. Even correctly signed, it
// must be refused: fail closed, never upgrade.
func oldFormatCookie(subject string, exp int64) string {
	key := []byte("test-key-test-key-test-key-32byt")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(subject + "|" + strconv.FormatInt(exp, 10)))
	return subject + "|" + strconv.FormatInt(exp, 10) + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func refuseCookie(t *testing.T, h http.Handler, value string) {
	t.Helper()
	w := do(h, http.MethodGet, "/notes/", "192.0.2.10:5555",
		withCookie(&http.Cookie{Name: sessionCookie, Value: value}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("forged cookie reached the console (%d): %q", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// TestPairBackslashOwnerDeviceCookieRoundTrips is the Windows pairing proof:
// with a qualified owner, redeeming a device ticket sets a cookie that
// survives net/http on every OS and identifies the request as device:alice.
// Tampered and pre-fix cookies refuse.
func TestPairBackslashOwnerDeviceCookieRoundTrips(t *testing.T) {
	injectBackslashOwner(t)
	h, store, sessions := backslashConsole(t)

	_, secret, err := store.issueTicketForPrincipal([]string{"notes"}, time.Hour, time.Minute, "alice")
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, http.MethodGet, pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), "192.0.2.10:5555", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redeem = %d, want 303 (body %q)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	cookie := sessionCookieFrom(t, w)

	// The subject behind the cookie still names the qualified owner and the
	// redeemed device; the MAC covers the decoded subject.
	subject, ok := sessions.Validate(cookie.Value)
	if !ok {
		t.Fatal("the redeemed cookie does not validate")
	}
	owner, deviceID, isDevice := splitDeviceSubject(subject)
	if !isDevice || owner != backslashOwner || deviceID == "" {
		t.Fatalf("device subject = %q, want qualified owner plus a device id", subject)
	}

	if got, code := principalOf(t, h, cookie); code != http.StatusOK || got != "device:alice" {
		t.Fatalf("paired request = %d %q, want 200 device:alice", code, got)
	}

	refuseCookie(t, h, tamperedValue(cookie.Value))
	old := oldFormatCookie(deviceSubject(backslashOwner, deviceID), time.Now().Add(time.Hour).Unix())
	if _, ok := sessions.Validate(old); ok {
		t.Fatal("a pre-fix device cookie validates — old cookies must fail closed")
	}
	refuseCookie(t, h, old)
}

// TestLoginBackslashOwnerSessionCookieRoundTrips is the operator half: the
// login session for a qualified owner round-trips through net/http and the
// request is identified as os:<owner>. Tampered and pre-fix cookies refuse.
func TestLoginBackslashOwnerSessionCookieRoundTrips(t *testing.T) {
	injectBackslashOwner(t)
	h, _, sessions := backslashConsole(t)

	// The operator types the bare name; SameUser admits it against the
	// qualified owner, and the session is minted for the canonical owner.
	form := url.Values{"user": {"testuser"}, "password": {"correct-horse"}}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.10:5555"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303 (body %q)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	cookie := sessionCookieFrom(t, w)

	if subject, ok := sessions.Validate(cookie.Value); !ok || subject != backslashOwner {
		t.Fatalf("operator subject = %q, %v, want %q", subject, ok, backslashOwner)
	}
	if got, code := principalOf(t, h, cookie); code != http.StatusOK || got != "os:"+backslashOwner {
		t.Fatalf("operator request = %d %q, want 200 os:%s", code, got, backslashOwner)
	}

	refuseCookie(t, h, tamperedValue(cookie.Value))
	old := oldFormatCookie(backslashOwner, time.Now().Add(time.Hour).Unix())
	if _, ok := sessions.Validate(old); ok {
		t.Fatal("a pre-fix operator cookie validates — old cookies must fail closed")
	}
	refuseCookie(t, h, old)
}
