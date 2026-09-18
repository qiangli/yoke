// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"
	"sort"
	"strings"
)

// Per-session panel scope: a paired phone is not silently a shell.
//
// THE PROBLEM. The console serves five panels — sprint, files, mb, meet,
// terminal. Anything that signs in on the LAN today gets ALL of them, so a
// phone on an untrusted network that acquires a credential acquires a SHELL
// (/term/, spawning a real bashy as this OS user) and the home directory
// (/files/). The authority granted is "everything this OS account can do"; the
// authority actually wanted, for the phone case, is "check on things from the
// couch".
//
// WHY IT IS CHEAP. The gate already resolves per-panel auth tiers on the FIRST
// PATH SEGMENT, and that is already documented as the security property: "a
// public app opens its own mount and nothing else ... so a public tile can
// never become a hole into the Terminal" (gate.go panelTier). A per-session
// scope rides the same resolution. It adds a set to the session, not a new
// authorization concept.
//
// WHY DEFAULT-DENY RATHER THAN A WARNING. The phone case is the one where the
// operator is least able to evaluate the risk in the moment — standing in a
// kitchen, scanning a code. A default that grants a shell and relies on the
// operator to have thought about it is the wrong way round. `--allow terminal`
// costs one flag when a shell is genuinely wanted.
//
// SCOPE IS ONLY EVER A NARROWING. It is applied AFTER the existing tier ladder
// and can only remove reach, never add it. A scoped session cannot become more
// privileged than the unscoped one it was derived from.

// defaultDeviceScope is what a QR pairing confers when --allow is not given:
// the read-and-communicate panels. Not terminal. Not files.
//
// Held as the canonical panel NAMES. Retired names are deliberately not
// expanded: a stored scope is authority, so an obsolete spelling must fail
// closed instead of silently granting a renamed surface.
//
// INBOX IS DELIBERATELY NOT HERE, even though it is read-only and looks like a
// sibling of the board. The board is public by construction — every agent on
// the host can already read every post on it — whereas the inbox panel is the
// aggregate of DIRECTED 1:1 mail for every name on the machine, which no single
// principal is otherwise entitled to read in one place. Default-deny applies to
// reach, not only to danger: `--allow inbox` costs one flag when a phone is
// genuinely meant to see the fleet's mail.
var defaultDeviceScope = []string{"meet", "mb", "sprint"}

// consoleWidePaths are reachable by ANY admitted session regardless of scope,
// because the launcher cannot render without them and a session that cannot
// sign out is worse than one that can.
func consoleWidePath(p string) bool {
	switch p {
	case "/", "/api/apps", "/api/session", "/api/login", "/api/logout",
		"/healthz", "/meta", "/login", "/favicon.ico",
		"/app.css", "/backgrounds.css", "/app.js", "/board.js", "/mb.js", "/inbox.js", "/term.js",
		"/vendor/xterm.css", "/vendor/xterm.js", "/vendor/xterm-addon-fit.js":
		return true
	}
	// The SPA's own assets live at the root and belong to no panel.
	return strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/static/")
}

// scopeSegment reduces a request path to the panel segment that owns it.
//
// The first-segment rule must hold for the scope check exactly as it does for
// the tier check, and /api/<panel> is the case where a naive first-segment read
// gets it wrong: /api/sprint would resolve to "api", which owns nothing, and an
// out-of-scope panel's data would be readable through its API while its page
// was refused. So "api" defers to the SECOND segment.
func scopeSegment(p string) string {
	seg := firstSegment(p)
	if seg != "api" {
		return seg
	}
	rest := strings.TrimPrefix(p, "/api")
	return firstSegment(rest)
}

// scopeSet is a session's allowed panel names, resolved to mount segments.
type scopeSet map[string]bool

// newScopeSet maps panel NAMES (what an operator types in --allow) onto the
// mount SEGMENTS the gate sees. They are allowed to differ — the terminal is
// "terminal" at /term/ — and keying on the name alone would leave the served
// path resolving to no scope at all, which is how a deny becomes an allow.
func (s *server) newScopeSet(names []string) scopeSet {
	set := scopeSet{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		set[n] = true
		if seg, ok := s.scopeSegments[n]; ok {
			set[seg] = true
		}
	}
	return set
}

// allows reports whether a scoped session may reach this path.
func (s *server) scopeAllows(scope []string, path string) bool {
	if len(scope) == 0 {
		// An empty scope is not "everything" — a device whose scope was lost
		// must fall closed. Only a nil scope (an operator session) skips this
		// check, and that is decided by the caller.
		return consoleWidePath(path)
	}
	if consoleWidePath(path) {
		return true
	}
	seg := scopeSegment(path)
	if seg == "" {
		return true // the launcher root
	}
	return s.newScopeSet(scope)[seg]
}

// normalizeScope lower-cases, de-duplicates and sorts a scope list, falling
// back to the default when empty.
func normalizeScope(names []string) []string {
	if len(names) == 0 {
		return append([]string(nil), defaultDeviceScope...)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return append([]string(nil), defaultDeviceScope...)
	}
	sort.Strings(out)
	return out
}

// ValidateScope rejects a panel name the console does not serve.
//
// Silently accepting an unknown name would be the worst outcome available: the
// operator believes they granted `--allow term` and the device is refused, or
// believes they narrowed a scope that in fact matched nothing.
func ValidateScope(names []string, panels []Panel) error {
	if len(names) == 0 {
		return nil
	}
	known := map[string]bool{}
	var list []string
	for _, p := range panels {
		known[strings.ToLower(p.Name)] = true
		list = append(list, p.Name)
		if seg := strings.Trim(p.Path, "/"); seg != "" {
			known[strings.ToLower(seg)] = true
		}
	}
	sort.Strings(list)
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if !known[n] {
			return fmt.Errorf("unknown panel %q; this console serves: %s",
				n, strings.Join(list, ", "))
		}
	}
	return nil
}
