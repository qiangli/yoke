package coopauth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// Policy is an app's trust configuration: the shared secret for HMAC, the admin
// allowlist (the app's own authority over who is admin), and loopback-owner
// trust. It is consumed two ways: Resolve() for a backend/proxy that also serves
// the on-host owner (loom), and Guard's middleware for a web app's routes.
type Policy struct {
	// Secret is the per-app SSO secret shared with outpost. Empty disables HMAC
	// (only safe when the upstream is loopback-only and RequireHMAC is false).
	Secret []byte
	// RequireHMAC rejects cloud-arrived requests lacking a valid signature. Keep
	// true whenever the upstream port is reachable beyond loopback.
	RequireHMAC bool
	// AdminEmails is the app-internal admin allowlist, keyed by lowercased email.
	// The cloud-stamped Remote-Groups is NOT trusted for admin (cloudbox marks
	// every sharee "admin"). Build with AdminSet. An EMPTY allowlist falls back
	// to the cloud tier — demo convenience only; real apps populate it.
	AdminEmails map[string]bool
	// LoopbackAdmin grants admin to on-host (loopback) callers — the machine
	// owner. Default off; a loopback-bound backend (loom) sets it true.
	LoopbackAdmin bool
	// Loopback{User,Email,Name} is the synthesized identity for a loopback
	// request that carries no vouch (the on-host owner). Defaults to "admin".
	LoopbackUser  string
	LoopbackEmail string
	LoopbackName  string
}

// AdminSet builds a case-insensitive admin allowlist from a list of emails.
func AdminSet(emails ...string) map[string]bool {
	m := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			m[e] = true
		}
	}
	return m
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// role decides admin vs user for an already-extracted identity.
func (p Policy) role(id Identity) string {
	if id.Loopback && p.LoopbackAdmin {
		return RoleAdmin
	}
	if len(p.AdminEmails) > 0 {
		if p.AdminEmails[strings.ToLower(id.Email)] || p.AdminEmails[strings.ToLower(id.User)] {
			return RoleAdmin
		}
		return RoleUser // populated allowlist is authoritative; cloud tier ignored
	}
	if id.CloudAdmin() { // empty allowlist => fall back to cloud tier (demo only)
		return RoleAdmin
	}
	return RoleUser
}

// Resolve is the ONE decision every backend runs: verify + extract + role. It
// handles both entrances a loopback-bound backend sees:
//   - Cloud-arrived (X-Forwarded-Prefix): HMAC-verified (per policy), identity =
//     the vouched email, role from the allowlist.
//   - Loopback (on-host owner): identity synthesized (or read from any vouch),
//     role = admin when LoopbackAdmin.
//
// ok=false means anonymous/refused. A direct NON-loopback request (a bare LAN
// hit that isn't the owner and carries no cloud vouch) is anonymous.
func (p Policy) Resolve(r *http.Request) (Identity, bool) {
	if ArrivedViaCloud(r) {
		if p.RequireHMAC || len(p.Secret) > 0 {
			if !VerifyOutpost(r, p.Secret) {
				return Identity{}, false
			}
		}
		id := rawIdentity(r)
		if !id.Authenticated() {
			return Identity{}, false
		}
		id.Username = Username(id.User)
		id.Loopback = false
		id.Role = p.role(id)
		return id, true
	}
	if !IsLoopbackAddr(r.RemoteAddr) {
		return Identity{}, false // direct non-loopback LAN, no vouch => anonymous
	}
	id := rawIdentity(r)
	if !id.Authenticated() { // synthesize the on-host owner
		id.User = firstNonEmpty(p.LoopbackUser, "admin")
		id.Email = firstNonEmpty(p.LoopbackEmail, id.User)
		id.Name = firstNonEmpty(p.LoopbackName, id.User)
	}
	id.Username = Username(id.User)
	id.Loopback = true
	id.Role = p.role(id)
	return id, true
}

// --- Web middleware (apps mount these on routes) ----------------------------

type ctxKey int

const identityKey ctxKey = 0

// Guard produces net/http middleware from a Policy. Unlike Resolve, the web
// middleware treats ONLY cloud-arrived callers as authenticated — a direct
// loopback hit to a web app is not a web session (its superadmin/local flow is
// RequireLocal + OS auth). Reuse one Guard for every protected route.
type Guard struct {
	Policy
	Log *slog.Logger
}

// verify establishes a trusted cloud Identity (with resolved Role) or false.
func (g *Guard) verify(r *http.Request) (Identity, bool) {
	if !ArrivedViaCloud(r) {
		return Identity{}, false
	}
	if g.RequireHMAC || len(g.Secret) > 0 {
		if !VerifyOutpost(r, g.Secret) {
			return Identity{}, false
		}
	}
	id := rawIdentity(r)
	if !id.Authenticated() {
		return Identity{}, false
	}
	id.Username = Username(id.User)
	id.Role = g.role(id)
	return id, true
}

// RequireAuth admits any cloud-vouched (and, per policy, HMAC-verified) caller.
func (g *Guard) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := g.verify(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

// RequireAdmin admits only verified callers the Policy resolves to admin — the
// app allowlist, not the cloud tier.
func (g *Guard) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := g.verify(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusForbidden)
			return
		}
		if !id.IsAdmin() {
			http.Error(w, "admin privileges required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

// RequireLocal admits only requests that did NOT arrive through the cloud tunnel
// — the app-side half of an lan_only_paths route, and where a real app's
// SUPERADMIN gate lives (prove local presence with OS auth before destructive
// actions; superadmin must never be grantable by cloud identity).
func (g *Guard) RequireLocal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ArrivedViaCloud(r) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IdentityOf returns the identity stored by RequireAuth/RequireAdmin.
func IdentityOf(r *http.Request) (Identity, bool) {
	id, ok := r.Context().Value(identityKey).(Identity)
	return id, ok
}
