// Package coopauth is the ONE shared implementation of the cloudbox/outpost
// cooperative-web-app identity + privilege model. Every dhnt app (loom, the
// hello-tessaro scaffold, classgo, …) resolves "who is this and are they admin"
// through this package, so the rule set lives in exactly one place: a security
// concern found in one app is fixed here and every consumer inherits the fix.
//
// It speaks only the PUBLIC wire contract (the Remote-*/X-Outpost-* headers
// outpost stamps — see outpost/docs/cooperative-web-apps.md) and depends only on
// the standard library, so a third-party app can import it directly.
//
// The model, in one paragraph: outpost vouches for a cloud caller with signed
// Remote-* headers; the caller's identity is their email, rendered to a valid,
// collision-free app username by Username(); privilege is decided by the APP's
// own admin allowlist (Policy.AdminEmails) plus loopback-owner trust — never by
// the cloud-stamped Remote-Groups, which cloudbox sets to "admin" for every
// sharee (trusting it would make every shared user an app admin).
package coopauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Wire header names stamped by outpost. Part of the wire contract — never rename
// them (outposts in the wild depend on them).
const (
	HdrForwardedPrefix = "X-Forwarded-Prefix" // e.g. /matrix/h/<host>/app/<name>
	HdrForwardedHost   = "X-Forwarded-Host"
	HdrForwardedProto  = "X-Forwarded-Proto"
	HdrRemoteUser      = "Remote-User"   // cloud-vouched OAuth email
	HdrRemoteEmail     = "Remote-Email"  // same identity, explicit email header
	HdrRemoteName      = "Remote-Name"   // display name
	HdrRemoteGroups    = "Remote-Groups" // "admin" | "user" — cloud tier, NOT app RBAC
	HdrIdentityTs      = "X-Outpost-Identity-Ts"
	HdrIdentitySig     = "X-Outpost-Identity-Sig"
)

// Roles the model resolves a caller to.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// clockSkew is the accepted signature age, matching the window outpost enforces.
const clockSkew = 60 * time.Second

// Identity is a resolved caller. User/Email/Name/Groups come off the vouch;
// Username is the sanitized, collision-free app-local name; Role is the app's
// verdict (admin|user) after applying the Policy.
type Identity struct {
	User     string // email (Remote-User / Remote-Email) — the canonical identity
	Email    string // real address (may differ from User only in exotic setups)
	Name     string // display name
	Groups   string // cloud tier: "admin" | "user" | "" — advisory only
	Username string // Username(User): valid, collision-free app username
	Role     string // RoleAdmin | RoleUser — the APP's decision (Policy)
	Loopback bool   // arrived on loopback (the on-host owner)
}

// Authenticated reports whether a caller was established at all.
func (id Identity) Authenticated() bool { return id.User != "" }

// IsAdmin reports the resolved app role, not the cloud tier.
func (id Identity) IsAdmin() bool { return id.Role == RoleAdmin }

// CloudAdmin reports the cloud tier stamp only. Advisory — never gate on it
// alone; the app allowlist (Policy) is authoritative. Kept for visibility/logs.
func (id Identity) CloudAdmin() bool { return id.Groups == RoleAdmin }

// Username maps an SSO identity (typically an email) to a valid app username
// for systems that reject '@' (Gitea, unix, …). It is a readable HANDLE, not the
// identity: the canonical identity is the email, which apps should match on
// directly (Gitea's reverse-proxy auth matches by X-WEBAUTH-EMAIL when no
// username is sent, so there is no username-collision surface). The mapping:
// lowercase, turn '@' and every disallowed rune into '-', collapse consecutive
// specials to one (Gitea forbids [-._]{2,}), and trim. alice@acme.com ->
// alice-acme.com. Because it is only a handle, an app that provisions accounts
// must still ensure uniqueness at CREATE time (disambiguate a taken handle);
// login never depends on it.
func Username(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	var b strings.Builder
	prevSpecial := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpecial = false
		case r == '-' || r == '_' || r == '.':
			if !prevSpecial { // collapse runs so we never emit [-._]{2,}
				b.WriteRune(r)
				prevSpecial = true
			}
		default: // '@' and anything else
			if !prevSpecial {
				b.WriteByte('-')
				prevSpecial = true
			}
		}
	}
	name := strings.Trim(b.String(), "-._")
	if name == "" {
		name = "user"
	}
	return name
}

// ArrivedViaCloud reports whether the request came through the cloudbox→outpost
// tunnel (X-Forwarded-Prefix present) vs. directly on the LAN/loopback. Outpost
// strips this header on direct requests and only stamps it for tunnel traffic,
// so it is the single reliable "came from the web" signal.
// SystemUser returns the local system-login handle — the OS user running the
// process, which (on a home node) IS the local admin. Reusable across custom
// apps that need "who owns this host" without a cloudbox/outpost dependency:
// they can make this identity an admin of their own local surface, exactly as
// loom promotes it to a site-admin. Empty only if no OS user env is set.
func SystemUser() string {
	for _, k := range []string{"USER", "LOGNAME", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// AdminIdentities returns the identities a custom app should treat as owner/admin,
// deduped: the explicit ones passed in (each may be comma-separated, e.g. a
// cloudbox-signup email plus extras) followed by the local SystemUser fallback.
// Standalone-safe — SystemUser alone yields a working single-operator admin with
// no cloud dependency; explicit identities layer on when a control plane supplies
// them. This is the shared resolver every custom app reuses instead of rederiving.
func AdminIdentities(explicit ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	for _, e := range explicit {
		add(e)
	}
	add(SystemUser())
	return out
}

func ArrivedViaCloud(r *http.Request) bool {
	return r.Header.Get(HdrForwardedPrefix) != ""
}

// IsLoopbackAddr reports whether a net address (host or host:port) is loopback.
func IsLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// rawIdentity extracts the stamped identity without verification.
func rawIdentity(r *http.Request) Identity {
	user := r.Header.Get(HdrRemoteUser)
	email := r.Header.Get(HdrRemoteEmail)
	if user == "" {
		user = email
	}
	if email == "" {
		email = user
	}
	return Identity{
		User:   user,
		Email:  email,
		Name:   r.Header.Get(HdrRemoteName),
		Groups: r.Header.Get(HdrRemoteGroups),
	}
}

// IdentityFrom extracts the stamped identity WITHOUT verification (Username is
// filled). Use it only to DISPLAY/log an unverified caller; call Resolve or a
// Guard method before trusting the values for access control.
func IdentityFrom(r *http.Request) Identity {
	id := rawIdentity(r)
	id.Username = Username(id.User)
	return id
}

// VerifyOutpost returns true only when the request carries a valid, fresh
// SSO-HMAC signature over the canonical identity payload. Empty secret => cannot
// verify (callers must treat that as "don't trust Remote-* beyond loopback").
//
//	payload = <Remote-User>\n<Remote-Groups>\n<X-Forwarded-Prefix>\n<Ts>
func VerifyOutpost(r *http.Request, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	prefix := r.Header.Get(HdrForwardedPrefix)
	ts := r.Header.Get(HdrIdentityTs)
	sigHex := r.Header.Get(HdrIdentitySig)
	if prefix == "" || ts == "" || sigHex == "" {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Now().Unix() - t; d > int64(clockSkew.Seconds()) || d < -int64(clockSkew.Seconds()) {
		return false
	}
	user := r.Header.Get(HdrRemoteUser)
	if user == "" {
		user = r.Header.Get(HdrRemoteEmail)
	}
	role := r.Header.Get(HdrRemoteGroups)
	payload := user + "\n" + role + "\n" + prefix + "\n" + ts
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// --- URL-prefix helpers (the other half of the cooperative contract) --------

// BasePrefix returns the mount prefix (no trailing slash), "" when reached
// directly. Emit `<base href="{prefix}/">` and build links relative.
func BasePrefix(r *http.Request) string {
	return strings.TrimRight(r.Header.Get(HdrForwardedPrefix), "/")
}

// PrefixPath joins the mount prefix with an absolute-rooted app path — for any
// absolute path emitted where outpost does NOT rewrite (JSON bodies, hand-built
// redirects, WebSocket URLs).
func PrefixPath(r *http.Request, rel string) string {
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return BasePrefix(r) + rel
}

// BaseHref returns the HTML <base href> value, always trailing-slashed.
func BaseHref(r *http.Request) string { return BasePrefix(r) + "/" }

// ExternalBase returns scheme://host as the browser sees it, from the forwarded
// headers (never Host/r.TLS, which reflect the loopback hop).
func ExternalBase(r *http.Request) string {
	proto := r.Header.Get(HdrForwardedProto)
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get(HdrForwardedHost)
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}
