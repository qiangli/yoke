// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/hostauth"
)

// sessionTTLSeconds bounds a login: short enough that a forgotten tab is not a
// standing grant, long enough not to interrupt work.
const sessionTTLSeconds = 12 * 60 * 60

// handleLoginPage renders the sign-in form.
//
// Server-rendered HTML rather than part of the SPA, for two reasons: it must
// work in a build whose bundle failed to load, and asking the browser to fetch
// and run a script before it can authenticate is a bootstrap it does not need.
func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r, "")
}

func (s *server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}

	owner := currentOSUser()
	errBlock := ""
	if errMsg != "" {
		errBlock = `<p class="err">` + htmlEscape(errMsg) + `</p>`
	}
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en" data-bashy-console="1"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<base href="` + htmlEscape(coopauth.BaseHref(r)) + `">
<title>Sign in &mdash; bashy apps</title>
<style>` + loginCSS + `</style>
</head><body>
<main class="login">
  <form method="post" action="api/login">
    <h1>bashy <b>apps</b></h1>
    <p class="sub">This host's shell and files. Sign in as
      <strong>` + htmlEscape(owner) + `</strong> with that account's password.</p>
    ` + errBlock + `
    <label>User<input name="user" autocomplete="username" autofocus value="` + htmlEscape(owner) + `"></label>
    <label>Password
      <span class="password-field">
        <input id="login-password" name="password" type="password" autocomplete="current-password">
        <button class="password-toggle" id="password-toggle" type="button" title="Show password"
          aria-label="Show password" aria-controls="login-password" aria-pressed="false" tabindex="-1">
          <svg class="eye" viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12Z"/><circle cx="12" cy="12" r="2.5"/></svg>
          <svg class="eye-off" viewBox="0 0 24 24" aria-hidden="true" hidden><path d="m3 3 18 18M10.6 6.2A11.5 11.5 0 0 1 12 6c6.5 0 10 6 10 6a18 18 0 0 1-2.3 3M6.6 6.6C3.6 8.4 2 12 2 12s3.5 6 10 6a10.8 10.8 0 0 0 4.1-.8M9.9 9.9a3 3 0 0 0 4.2 4.2"/></svg>
        </button>
      </span>
    </label>
    <button class="btn primary" type="submit">Sign in</button>
  </form>
</main>
<script>
(() => {
  const input = document.getElementById("login-password");
  const toggle = document.getElementById("password-toggle");
  const eye = toggle.querySelector(".eye");
  const eyeOff = toggle.querySelector(".eye-off");
  // Drive the ATTRIBUTE, not the property. These are <svg> elements, so they
  // are SVGElement -- and hidden is an IDL attribute of HTMLElement, which
  // SVGElement does not inherit. Assigning that property on an icon would
  // therefore set a meaningless JS expando, leave the real attribute
  // untouched, and the two icons would never swap.
  const setShown = (el, shown) => {
    if (shown) el.removeAttribute("hidden");
    else el.setAttribute("hidden", "");
  };
  toggle.addEventListener("click", () => {
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    const label = show ? "Hide password" : "Show password";
    toggle.title = label;
    toggle.setAttribute("aria-label", label);
    toggle.setAttribute("aria-pressed", String(show));
    setShown(eye, !show);
    setShown(eyeOff, show);
  });
})();
</script>
</body></html>`))
}

// loginCSS is inlined rather than linked.
//
// The gate closes everything except the login route itself, so a <link> to
// app.css is redirected to /login and the form renders unstyled — which is
// exactly what shipped the first time this was tried. Inlining also honours the
// reason the page is server-rendered at all: signing in must not depend on
// fetching anything else first.
const loginCSS = `
:root{--bg:#f2f2f5;--fg:#18181b;--card:#fff;--muted:#71717a;--line:#e4e4e7;--primary:#2563eb}
@media (prefers-color-scheme:dark){:root{--bg:#09090b;--fg:#fafafa;--card:#18181b;--muted:#a1a1aa;--line:#27272a;--primary:#60a5fa}}
*{box-sizing:border-box}
body{margin:0;min-height:100svh;display:flex;align-items:center;justify-content:center;
 background:var(--bg);color:var(--fg);padding:2rem 1.25rem;
 font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
form{width:100%;max-width:21rem;display:flex;flex-direction:column;gap:.7rem;
 background:var(--card);border:1px solid var(--line);border-radius:1rem;
 padding:1.6rem 1.5rem;box-shadow:0 12px 36px rgba(20,30,60,.18)}
h1{margin:0;font-size:1.15rem;font-weight:800;letter-spacing:-.02em}
h1 b{font-weight:500;color:var(--muted);font-size:.8rem;margin-left:.3rem}
.sub{margin:-.3rem 0 .4rem;color:var(--muted);font-size:.82rem}
label{display:flex;flex-direction:column;gap:.25rem;font-size:.75rem;color:var(--muted);
 font-weight:600;text-transform:uppercase;letter-spacing:.05em}
input{font:inherit;font-size:.92rem;text-transform:none;letter-spacing:0;color:var(--fg);
 padding:.5rem .65rem;border-radius:.5rem;border:1px solid var(--line);background:var(--bg)}
input:focus{outline:none;box-shadow:0 0 0 2px var(--primary)}
.password-field{position:relative;display:block}
.password-field input{width:100%;padding-right:2.6rem}
.password-toggle{position:absolute;right:.15rem;top:50%;transform:translateY(-50%);margin:0;padding:.4rem;
 display:flex;align-items:center;justify-content:center;border:0;background:transparent;color:var(--muted)}
.password-toggle:hover{color:var(--fg)}
.password-toggle:focus-visible{outline:2px solid var(--primary);outline-offset:1px}
.password-toggle svg{width:1.05rem;height:1.05rem;fill:none;stroke:currentColor;stroke-width:1.8;
 stroke-linecap:round;stroke-linejoin:round}
/* The UA's [hidden]{display:none} is not reliable for an <svg>: the HTML UA
   stylesheet declares a default XHTML namespace, so it does not dependably
   match an element in the SVG namespace, and BOTH icons render at once. State
   the rule in the author sheet, where the svg type selector matches inline SVG
   regardless. */
.password-toggle svg[hidden]{display:none}
button{margin-top:.5rem;font:inherit;font-size:.92rem;font-weight:600;padding:.55rem;
 border-radius:.5rem;border:0;background:var(--primary);color:#fff;cursor:pointer}
.err{margin:0;font-size:.82rem;color:#ef4444}
`

// handleLogin verifies an OS password and mints a session.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Rate limit on the PEER address. Not X-Forwarded-For: that is
	// attacker-supplied on exactly the path this protects.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.limiter != nil && !s.limiter.Allow(host) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, "Malformed request.")
		return
	}
	user := strings.TrimSpace(r.FormValue("user"))
	pass := r.FormValue("password")

	// Only the OS user this process runs as may sign in. Admitting anyone else
	// would check the password against one account and grant a shell running as
	// another — the terminal here is this process's user, whoever authenticated.
	owner := currentOSUser()
	if !hostauth.SameUser(user, owner) {
		s.renderLogin(w, r, "Sign in as "+owner+" on this host.")
		return
	}
	auth := s.auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}
	if err := auth.Authenticate(user, pass); err != nil {
		s.renderLogin(w, r, "Incorrect password.")
		return
	}

	// A loopback console mints NO sessions: cmd.go builds the store only when it
	// binds off-loopback. /login is routed regardless, so without this guard a
	// CORRECT password reached Mint on a nil *websession.Store and panicked --
	// the page after signing in was blank, while a WRONG password returned a
	// tidy 401. That asymmetry is why it survived every negative test.
	//
	// There is nothing to sign in TO here: the gate already admits the machine
	// owner on loopback. Send them where they were trying to go.
	if s.sessions == nil {
		http.Redirect(w, r, coopauth.PrefixPath(r, "/"), http.StatusSeeOther)
		return
	}

	cookie, err := s.sessions.Mint(hostauth.BareUsername(user))
	if err != nil {
		s.renderLogin(w, r, "Could not start a session.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookie,
		Path:     coopauth.BasePrefix(r) + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.Header.Get(coopauth.HdrForwardedProto) == "https" || r.TLS != nil,
		MaxAge:   sessionTTLSeconds,
	})
	http.Redirect(w, r, coopauth.PrefixPath(r, "/"), http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.sessions != nil {
		s.sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: coopauth.BasePrefix(r) + "/", MaxAge: -1,
	})
	http.Redirect(w, r, coopauth.PrefixPath(r, "/login"), http.StatusSeeOther)
}

// currentOSUser is the account this process runs as, and therefore the only
// account that may sign in — see handleLogin.
func currentOSUser() string {
	u, err := hostauth.CurrentUser()
	if err != nil {
		return ""
	}
	return u
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;",
		`"`, "&quot;", "'", "&#39;").Replace(s)
}
