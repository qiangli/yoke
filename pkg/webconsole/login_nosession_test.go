// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type okAuth struct{ password string }

func (a okAuth) Authenticate(_, pass string) error {
	if pass == a.password {
		return nil
	}
	return errInvalidForTest
}

var errInvalidForTest = &authErr{}

type authErr struct{}

func (*authErr) Error() string { return "invalid" }

// A loopback console mints NO sessions: cmd.go builds the store only when it
// binds off-loopback. /login is still routed there, so a CORRECT password
// reached s.sessions.Mint on a nil *websession.Store and panicked — the page
// after signing in was blank, while a wrong password returned a tidy 401. That
// asymmetry is why it survived: every negative test passed.
func TestLoginWithoutASessionStoreDoesNotPanic(t *testing.T) {
	h := newTestHandler(t, Options{Auth: okAuth{password: "correct-horse"}})

	form := url.Values{"user": {currentOSUser()}, "password": {"correct-horse"}}
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r) // must not panic

	if w.Code != http.StatusSeeOther {
		t.Fatalf("login on a session-less console = %d, want 303 to the console (body %q)",
			w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q — the gate already admits the owner here", loc, "/")
	}
}
