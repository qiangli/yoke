// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package websession is the stateless HMAC session cookie and per-IP login
// throttle a locally-served web surface needs.
//
// COPIED from outpost internal/agent/adminui/{sessions,login_limiter}.go, which
// is not importable (internal/). The two are deliberate copies rather than a
// shared module: see dhnt docs for the decision. If you fix something here, look
// there too — nothing reports the divergence.
package websession

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store mints and validates HMAC-signed admin session cookies.
//
// Cookies are stateless: `v1|<base64url(subject)>|<expUnixSecs>|<sigBase64>`.
// Validation decodes the subject first, then re-HMACs the decoded subject
// with the server's secret and compares. This means the session SURVIVES
// process restarts (the secret is loaded from the FileConfig on every
// boot), so flipping a built-in toggle — which re-execs the binary — no
// longer logs the admin user out.
//
// The subject is base64url-encoded so the cookie value only ever uses valid
// cookie-value bytes on every OS. A Windows account name such as
// `DOMAIN\user` carries a backslash, which Go's net/http drops from Cookie
// values — silently breaking the MAC and locking every session out with a
// 403. The MAC covers the DECODED subject, so a tampered encoding cannot
// upgrade privileges, and a pre-v1 cookie (`user|exp|sig`) has the wrong
// shape and fails closed (re-pair or log in again).
//
// Revocation is best-effort: an in-memory set tracks cookies that were
// explicitly logged out. The set is lost on restart, but the cookie that
// was revoked is also gone from the user's browser (we sent MaxAge=-1),
// so this only matters if someone copied the cookie elsewhere — and in
// that case the bounded 1h TTL caps the exposure.
type Store struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	revoked map[string]time.Time
}

// NewStore builds a session store with the given TTL and signing
// key. An empty secret causes a one-shot random key to be generated —
// useful for tests; production paths should pass a persisted key so
// sessions outlive process restarts.
func NewStore(ttl time.Duration, secret []byte) *Store {
	if len(secret) == 0 {
		var b [32]byte
		_, _ = rand.Read(b[:])
		secret = b[:]
	}
	return &Store{
		secret:  secret,
		ttl:     ttl,
		now:     time.Now,
		revoked: map[string]time.Time{},
	}
}

// cookieVersion marks the encoded-subject format. Any cookie without it is
// pre-v1 and fails closed in Validate — never upgraded, never admitted.
const cookieVersion = "v1"

// Mint returns a signed session cookie. The user string is opaque to the
// store — it is base64url-encoded into the cookie and the decoded subject
// is surfaced from Validate. Must not contain '|' (the MAC input
// separator). Practically the callers pass an OS account name
// (`DOMAIN\user` on Windows) or a `user#dev:<device>` device subject.
func (s *Store) Mint(user string) (string, error) {
	if strings.ContainsRune(user, '|') {
		// Defensive: refuse separator chars so the MAC input stays unambiguous.
		return "", errSeparatorInUser
	}
	exp := strconv.FormatInt(s.now().Add(s.ttl).Unix(), 10)
	sig := s.sign(user + "|" + exp)
	return cookieVersion + "|" + base64.RawURLEncoding.EncodeToString([]byte(user)) + "|" + exp + "|" + sig, nil
}

// Validate returns the user owning the cookie if signature checks pass
// and the cookie isn't expired or revoked.
func (s *Store) Validate(cookie string) (string, bool) {
	if cookie == "" {
		return "", false
	}
	// Reject revoked-in-this-process cookies before any HMAC work.
	s.mu.Lock()
	_, gone := s.revoked[cookie]
	s.mu.Unlock()
	if gone {
		return "", false
	}
	parts := strings.SplitN(cookie, "|", 4)
	if len(parts) != 4 || parts[0] != cookieVersion {
		// Wrong shape — including every pre-v1 `user|exp|sig` cookie.
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	user, expStr, sig := string(raw), parts[2], parts[3]
	// The MAC covers the DECODED subject: swapping the encoding changes the
	// subject and therefore the MAC input, so it cannot slip past.
	want := s.sign(user + "|" + expStr)
	// Constant-time compare to avoid timing leaks of the signature.
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "", false
	}
	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", false
	}
	if s.now().Unix() > expUnix {
		return "", false
	}
	return user, true
}

// Revoke best-effort invalidates a cookie for this process's lifetime.
// Stateless cookies can't be truly recalled; the revocation set + the
// browser-side cookie clear (Set-Cookie MaxAge=-1) together give logout
// the user-visible behavior they expect.
func (s *Store) Revoke(cookie string) {
	if cookie == "" {
		return
	}
	s.mu.Lock()
	s.revoked[cookie] = s.now()
	s.mu.Unlock()
}

func (s *Store) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// errSeparatorInUser is the sentinel for the unreachable Mint() guard.
// Defined as a package var to avoid stringly-typed errors in callers.
var errSeparatorInUser = mintError("user name may not contain '|'")

type mintError string

func (e mintError) Error() string { return string(e) }
