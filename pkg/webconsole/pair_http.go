// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/policy/audit"
)

// pairRedeemPath is where a scanned QR lands.
const pairRedeemPath = "/pair/redeem"

// handlePairRedeem turns a one-time ticket into a device-scoped session.
//
// It is an OPEN path (see isOpenPath) for the same reason /login is: it is
// where an identity is established. What keeps it safe is that the ticket is
// single-use, short-lived, hashed at rest, rate-limited by peer address, and
// confers a SCOPE rather than the operator's full authority.
func (s *server) handlePairRedeem(w http.ResponseWriter, r *http.Request) {
	// Rate limit on the PEER address, not X-Forwarded-For: that header is
	// attacker-supplied on exactly the path this protects.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.limiter != nil && !s.limiter.Allow(host) {
		http.Error(w, "too many pairing attempts", http.StatusTooManyRequests)
		return
	}
	if s.pairing == nil || s.sessions == nil {
		s.pairFailure(w, host, "pairing is not enabled on this console",
			"This console is not accepting pairings. Start it with `bashy app serve --bind <lan-ip> --pair`.",
			http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if v := q.Get("v"); v != "" && v != pairQRVersion {
		// A future QR carrying, say, a certificate fingerprint must not be
		// silently redeemed by a server that would ignore the new field.
		s.pairFailure(w, host, "unsupported pairing payload version "+v,
			fmt.Sprintf("This code was made for a newer bashy (payload v%s; this console speaks v%s). Upgrade bashy on the host, or generate a fresh code.", v, pairQRVersion),
			http.StatusBadRequest)
		return
	}
	secret := strings.TrimSpace(q.Get("t"))
	if secret == "" {
		s.pairFailure(w, host, "pairing request carried no ticket",
			"That link is missing its pairing ticket. Scan the code again from `bashy app pair`.",
			http.StatusBadRequest)
		return
	}

	name := deviceNameFrom(r)
	d, err := s.pairing.redeem(secret, name, truncate(r.UserAgent(), 200))
	if err != nil {
		status := http.StatusForbidden
		human := "That pairing code was not accepted. Generate a fresh one with `bashy app pair`."
		switch {
		case errors.Is(err, errTicketUsed):
			human = "That code has already been used. A pairing code works exactly once — generate a fresh one with `bashy app pair`."
		case errors.Is(err, errTicketStale):
			human = "That code has expired. Generate a fresh one with `bashy app pair`."
		case errors.Is(err, errNoTicket):
			human = "That code is not recognised by this host. Generate a fresh one with `bashy app pair`."
		}
		s.pairFailure(w, host, "pair redemption refused: "+err.Error(), human, status)
		return
	}

	cookie, err := s.sessions.Mint(deviceSubject(currentOSUser(), d.ID))
	if err != nil {
		s.pairFailure(w, host, "could not mint a device session: "+err.Error(),
			"The host could not start a session for this device.", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookie,
		Path:     coopauth.BasePrefix(r) + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.Header.Get(coopauth.HdrForwardedProto) == "https" || r.TLS != nil,
		// The cookie must not outlive the device record it names. The gate
		// re-checks the device on every request anyway, so this is defence in
		// depth rather than the enforcement point.
		MaxAge: int(time.Until(d.Expires).Seconds()),
	})
	auditPairEvent("pair.redeemed", map[string]string{
		"device": d.ID, "scope": strings.Join(d.Scope, ","),
		"expires": d.Expires.Format(time.RFC3339), "peer": host,
	})
	http.Redirect(w, r, coopauth.PrefixPath(r, landingFor(d.Scope)), http.StatusSeeOther)
}

// pairFailure records the refusal and answers in words the person holding the
// phone can act on. A failed redemption is a security event: it is the shape a
// guessing attack takes.
func (s *server) pairFailure(w http.ResponseWriter, peer, reason, human string, status int) {
	auditPairEvent("pair.refused", map[string]string{"reason": reason, "peer": peer})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pairing failed &mdash; bashy apps</title><style>` + loginCSS + `</style></head>
<body><main class="login"><form onsubmit="return false">
<h1>bashy <b>apps</b></h1><p class="err">` + htmlEscape(human) + `</p>
</form></main></body></html>`))
}

// landingFor picks the page a freshly paired device opens: the LAUNCHER.
//
// It used to open the first panel in scope (sprint, then mb, meet, files,
// terminal), which dropped a phone straight into the board with no sense of
// where it was or what else it could reach. The launcher is the console's one
// nav with everything deep-linked beneath it, it lists exactly the panels this
// device is scoped to, and it is a consoleWidePath — reachable by ANY admitted
// session regardless of scope, so this can never land on a refusal.
//
// The scope argument stays: the panels a device may reach still come from it,
// and a future landing rule would need it again.
func landingFor(_ []string) string {
	return "/"
}

// deviceNameFrom guesses a human label from the User-Agent so `apps devices`
// reads as "iPhone" rather than a random id. Best-effort by construction: a
// wrong guess is cosmetic, and the id is always shown beside it.
func deviceNameFrom(r *http.Request) string {
	ua := r.UserAgent()
	for _, probe := range []struct{ needle, label string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"},
		{"Macintosh", "Mac"}, {"Windows", "Windows"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, probe.needle) {
			return probe.label
		}
	}
	return "device"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

// auditPairEvent appends one pairing record to the tamper-evident audit chain.
//
// UNLIKE command auditing, this is NOT gated on $BASHY_AUDIT. That switch
// exists to bound the volume and privacy cost of recording every command a
// shell dispatches. Pairing is the opposite kind of event: rare, and the exact
// thing an operator needs to be able to review afterwards — who was granted
// reach to this host, with what scope, and when it was taken away. A grant
// nobody can audit is a grant nobody can supervise.
//
// Best-effort: a failed write must never fail the request that was being
// recorded, because the alternative is an operator unable to pair a phone
// because a log directory is read-only.
func auditPairEvent(action string, fields map[string]string) {
	path := pairAuditPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	w, err := audit.Open(path)
	if err != nil {
		return
	}

	argv := []string{action}
	for _, k := range sortedKeys(fields) {
		argv = append(argv, k+"="+fields[k])
	}
	host, _ := os.Hostname()
	_, _ = w.Append(audit.Record{
		// Append fills Seq/PrevHash/Hash but NOT Time; an audit record whose
		// timestamp is the empty string cannot answer "when was this device
		// let in?", which is most of the point.
		Time:   time.Now().UTC().Format(time.RFC3339Nano),
		Actor:  audit.ActorFromEnv(),
		Action: action,
		Argv:   argv,
		Binary: "bashy app",
		Host:   host,
		// Every pairing event is an authorization decision about this host's
		// shell and files.
		Effects: []string{"net:listen", "auth:grant"},
	})
}

// pairAuditPath mirrors bashy's own ladder ($BASHY_AUDIT as a path, then
// $BASHY_HOME, then ~/.bashy) so pairing records land in the same chain
// `bashy audit` reads. Unlike bashy's, a boolean-off value does not suppress
// it: see auditPairEvent.
func pairAuditPath() string {
	v := strings.TrimSpace(os.Getenv("BASHY_AUDIT"))
	switch strings.ToLower(v) {
	case "", "0", "1", "true", "false", "on", "off", "yes", "no":
		// a boolean or unset: use the default path
	default:
		return v
	}
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "audit", "audit.jsonl")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".bashy", "audit", "audit.jsonl")
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
