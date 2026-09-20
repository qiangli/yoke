// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/hostauth"
	"github.com/qiangli/yoke/pkg/meet"
	"github.com/qiangli/yoke/pkg/websession"
	"github.com/qiangli/yoke/pkg/webterm"
)

// otelServiceName is the launcher's own span/service identity. It is a real hop
// on the trace plane (browser -> cloudbox -> outpost -> apps -> panel), so it
// names itself rather than borrowing a panel's name.
const otelServiceName = "bashy-apps"

// sessionCookie is the console's session cookie name.
const sessionCookie = "bashy_console"

// DefaultPort is the console's default port: 22749, the telephone-keypad
// encoding of BASHY (2-2-7-4-9). It is declared to the atlas so
// `commands --view web` and the console agree, and it is the port a
// Settings-page pairing QR points a phone at unless --port overrides it.
// Existing explicit 8639 deployments remain supported through --port.
const DefaultPort = 22749

// Options configures a console.
type Options struct {
	// Ctx is the SERVER's lifetime, not a request's — background work started by
	// a panel outlives the request that asked for it.
	Ctx context.Context

	// Scope is the filesystem root the Files panel is confined to. Empty means
	// the working directory.
	Scope string
	// AllowWrite enables the Files panel's write operations. Default read-only.
	AllowWrite bool

	// RequireLogin disables the ungated-loopback row of the gate. Set
	// automatically when binding a non-loopback address.
	RequireLogin bool
	// Sessions validates console cookies; nil disables cookie auth.
	Sessions *websession.Store
	// Auth verifies an OS password. nil means hostauth.DefaultAuthenticator().
	Auth hostauth.Authenticator

	// Terminal configures the shell behind the Terminal panel.
	Terminal webterm.Options

	// Apps are --app specs (`<bin>` or `<bin>@<port>`) naming third-party
	// programs to publish as tiles. Each is probed with `<bin> meta --json`.
	//
	// This is OPERATOR ARGV, exactly like Disable. bashy never scans a
	// directory for binaries and never takes an app list off the network,
	// because probing one means EXEC'ing it.
	Apps []string
	// ProbeApp is the exec seam. Nil means ProbeApp; tests inject a stub so a
	// unit test never spawns a process.
	ProbeApp ProbeFunc
	// AppAuth is explicit operator policy keyed by third-party mount. Metadata
	// may request presentation facts but cannot open a LAN-facing route itself.
	AppAuth map[string]string

	// Panels overrides discovery. Nil means Discover().
	Panels []Panel

	// Port is the port this console listens on, used to build the address a
	// Settings-page pairing QR points a phone at. Zero means DefaultPort. It is
	// the ACTUAL bound port so an explicit --port override is reflected in the
	// code the phone scans rather than a stale default.
	Port int

	// Pairing enables QR device pairing: `bashy app pair` mints a one-time
	// ticket, the phone redeems it at /pair/redeem, and the resulting session
	// is device-scoped. Off unless the operator asked for it — it only makes
	// sense on a LAN-bound console, and turning it on silently would open a
	// redemption route nobody requested.
	Pairing bool
	// PairStorePath overrides the pairing document's location. Tests set it;
	// production leaves it empty and gets the console's own state directory.
	PairStorePath string

	// LookStorePath overrides the look settings document's location (the
	// global "Open apps" mode lives there). Tests set it; production leaves it
	// empty and gets the console's own state directory. The store performs no
	// I/O until first use, and a missing or corrupt document serves defaults.
	LookStorePath string

	// Disable names panels to leave out entirely — not greyed out, not listed:
	// absent from the tile list AND unrouted.
	//
	// This exists because outpost publishes the console under ONE app name, and
	// HostShare grants are per app name. Without it, sharing the console would
	// mean sharing the Terminal — which spawns a real bashy as this OS user —
	// and that is strictly more authority than sharing outpost's own `shell`
	// app was. A consolidation must not widen a grant by accident.
	Disable []string
}

// disabled reports whether --disable named this panel, by its NAME or mount.
func (o Options) disabled(p Panel) bool {
	mount := strings.Trim(p.Path, "/")
	for _, d := range o.Disable {
		d = strings.TrimSpace(d)
		if strings.EqualFold(d, p.Name) || (mount != "" && strings.EqualFold(d, mount)) {
			return true
		}
	}
	return false
}

type server struct {
	opts         Options
	guard        *coopauth.Guard
	sessions     *websession.Store
	auth         hostauth.Authenticator
	limiter      *websession.Limiter
	requireLogin bool
	port         int
	panels       []Panel
	panelAuth    map[string]string // mount segment -> auth tier
	// scopeSegments maps a panel NAME (what an operator types in --allow) to
	// its mount SEGMENT (what the gate sees). They differ — the terminal is
	// "terminal" at /term/ — and conflating them turns a deny into an allow.
	scopeSegments map[string]string
	pairing       *pairStore
	look          *lookStore
	cloudPair     *cloudPairer // the Cloud section's one action (cloud.go)
	probes        probeCache
	inboxes       inboxCache
	boards        boardCache
}

// consoleGuard mirrors meet's: an SSO secret, when configured, makes the vouch
// HMAC-verified rather than merely present.
func consoleGuard() *coopauth.Guard {
	return &coopauth.Guard{Policy: coopauth.Policy{}}
}

// Handler returns the console as an http.Handler, plus a closer for whatever it
// had to start. It is the embedding seam: `apps serve` is a thin caller,
// and outpost or a desktop shell is the same shape.
func Handler(opts Options) (http.Handler, func() error, error) {
	_, h, closer, err := newHandler(opts)
	return h, closer, err
}

// newHandler is Handler plus the server it built. Tests use it to reach state
// that has no HTTP surface — the board cache, which must be seedable so a test
// never runs the real collector and its subprocess fan-out.
func newHandler(opts Options) (*server, http.Handler, func() error, error) {
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	s := &server{
		opts:         opts,
		guard:        consoleGuard(),
		sessions:     opts.Sessions,
		auth:         opts.Auth,
		requireLogin: opts.RequireLogin,
		port:         opts.Port,
		panels:       opts.Panels,
	}
	if s.port == 0 {
		s.port = DefaultPort
	}
	if s.requireLogin {
		if s.sessions == nil {
			// Ephemeral key: sessions do not survive a restart, which is the
			// right default for a process started by hand. runServe persists one.
			s.sessions = websession.NewStore(12*time.Hour, nil)
		}
		// Burst 5, then one attempt per 12s. PAM/dscl is already slow on the
		// success path; this bounds a LAN brute-forcer on the failure path.
		s.limiter = websession.NewLimiter(5, 12*time.Second)
	}
	if opts.Pairing {
		path := opts.PairStorePath
		if path == "" {
			p, err := pairPath()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("apps: pairing store: %w", err)
			}
			path = p
		}
		s.pairing = newPairStore(path)
		if s.sessions == nil {
			// Pairing without a session store would mint cookies nothing can
			// validate. Fail closed rather than pair into a void.
			return nil, nil, nil, errors.New("apps: pairing needs a session store (it is enabled by an off-loopback bind)")
		}
		if s.limiter == nil {
			s.limiter = websession.NewLimiter(5, 12*time.Second)
		}
	}
	// The look settings (global "Open apps" mode) are always present: no I/O
	// happens here, a missing or corrupt document serves the safe default, and
	// the chrome below needs a definite mode on every page render.
	lookFile := opts.LookStorePath
	if lookFile == "" {
		if p, err := lookPath(); err != nil {
			// A console that cannot locate its state dir still serves; it
			// just cannot remember a preference across restarts.
			slog.Warn("apps: console look store has no home; settings will not persist", "err", err)
		} else {
			lookFile = p
		}
	}
	s.look = newLookStore(lookFile)

	if s.panels == nil {
		s.panels = Discover()
		if len(opts.Apps) > 0 {
			apps, errs := discoverApps(opts.Ctx, opts.Apps, opts.ProbeApp, TakenMounts(s.panels), opts.AppAuth)
			for _, err := range errs {
				// Reported, never silent: a tile that quietly vanished looks
				// exactly like one nobody asked for.
				slog.Error("apps: skipping --app", "err", err)
			}
			s.panels = append(s.panels, apps...)
		}
	}
	if len(opts.Disable) > 0 {
		kept := s.panels[:0]
		for _, p := range s.panels {
			if !opts.disabled(p) {
				kept = append(kept, p)
			}
		}
		s.panels = kept
	}
	// on reports whether a panel survived, so a disabled one is never ROUTED
	// either. Dropping it from the tile list alone would leave the surface
	// reachable to anyone who typed the path.
	on := map[string]bool{}
	s.panelAuth = map[string]string{}
	s.scopeSegments = map[string]string{}
	for _, p := range s.panels {
		on[p.Name] = true
		tier := p.Auth
		if tier == "" {
			tier = AuthSystem
		}
		if seg := strings.Trim(p.Path, "/"); seg != "" {
			s.scopeSegments[strings.ToLower(p.Name)] = seg
		}
		// Key on the mount SEGMENT, not the name: they are allowed to differ
		// (the terminal is "terminal" at /term/), and keying on the name would
		// leave the served path resolving to no tier at all.
		if seg := strings.Trim(p.Path, "/"); seg != "" {
			s.panelAuth[seg] = tier
		}
	}

	mux := http.NewServeMux()

	// Ungated: a liveness probe that needs an identity is a probe that cannot run.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/apps", s.handleApps)
	mux.HandleFunc("GET /api/session", s.handleSession)
	// The look settings: GET is the structured projection, PUT is the one
	// writer. Gated by the same console ladder as every other /api route —
	// a preference write is a console write, not a public one.
	mux.HandleFunc("GET /api/look", s.handleLookGet)
	mux.HandleFunc("PUT /api/look", s.handleLookPut)
	// The Cloud section: pairing state, and the one action that makes it.
	mux.HandleFunc("GET /api/cloud", s.handleCloudGet)
	mux.HandleFunc("POST /api/cloud/pair", s.handleCloudPair)
	// The external self-description. Ungated (see isOpenPath) and deliberately
	// a projection, not the internal Panel.
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("GET /meta/{app}", s.handleMetaApp)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	// The Settings-page pairing mint is registered UNCONDITIONALLY so a console
	// without --pair can still answer it — with a fail-closed body naming the
	// restart command — rather than falling through to the SPA and returning
	// HTML to a fetch() that expected JSON. handlePairMint checks s.pairing.
	mux.HandleFunc("POST /api/pair", s.handlePairMint)
	if s.pairing != nil {
		mux.HandleFunc("GET "+pairRedeemPath, s.handlePairRedeem)
	}

	// Deep links from docs/agent-interaction-surfaces-design.md keep working,
	// each only while its target panel is enabled. The room is mounted at
	// /meet/ now — the app is called Meet everywhere a person sees it, and
	// "Relay" on the address bar of a page titled Meet is the kind of
	// inconsistency a reader has to stop and resolve.
	if on["meet"] {
		mux.Handle("GET /meet", redirectTo("/meet/"))
	}

	// The terminal is served by the launcher itself rather than mounted, so it
	// gets a full page of its own with the launcher's <base href> — every app
	// opens as a real browser page, none of them framed inside another.
	// One pattern, dispatched inside: net/http's mux rejects "GET /term/"
	// alongside "/term/ws" as conflicting, and the socket must accept the
	// upgrade on any method anyway.
	if on["terminal"] {
		termSocket := webterm.SocketHandler(s.opts.Terminal)
		mux.HandleFunc("/term/", func(w http.ResponseWriter, r *http.Request) {
			if path.Base(r.URL.Path) == "ws" {
				termSocket.ServeHTTP(w, r)
				return
			}
			s.handleTermPage(w, r)
		})
		mux.Handle("GET /term", redirectTo("/term/"))
		mux.Handle("GET /shell", redirectTo("/term/"))
	}

	// The board, served the same way and for the same reason: its own page at
	// the launcher's root, so it inherits app.css and the header chrome. The
	// atlas declares the TILE (Discover picks it up); panelHandler returns nil
	// for it, exactly as it does for the terminal, so the panel loop below does
	// not also mount it under coopauth.Mount and take the <base href> with it.
	if on["mb"] {
		mux.HandleFunc("GET /mb/", s.handleMBPage)
		mux.Handle("GET /mb", redirectTo("/mb/"))
		mux.HandleFunc("GET /api/mb", s.handleMBList)
		mux.HandleFunc("POST /api/mb", s.handleMBSend)
		mux.HandleFunc("GET /api/mb/{seq}/viewers", s.handleMBViewers)
	}

	// The inbox, same shape again — and read-only by construction on BOTH
	// sides: there is no POST here at all, and the handlers behind these routes
	// go through pkg/bus's inspection API, which never advances a cursor, never
	// materializes a pending record and never opens a subscription. A page that
	// wrote would consume mail belonging to an agent that had not read it yet.
	if on["inbox"] {
		mux.HandleFunc("GET /inbox/", s.handleInboxPage)
		mux.Handle("GET /inbox", redirectTo("/inbox/"))
		mux.HandleFunc("GET /api/inbox", s.handleInboxRoster)
		mux.HandleFunc("GET /api/inbox/{name}", s.handleInboxList)
		// The ONE write, and it takes NO NAME: it marks mail read in the
		// caller's own inbox and there is no parameter through which another
		// could be named. See handleInboxMarkRead — the absence IS the
		// enforcement.
		mux.HandleFunc("POST /api/inbox/read", s.handleInboxMarkRead)
	}

	// The steward board, same shape again: the tile is an atlas declaration,
	// the page is served at the launcher's root, and panelHandler claims
	// nothing. Read-only by construction: Sprint's browser projection must not
	// acquire the command's lifecycle mutations merely because it shares a noun.
	if on["sprint"] {
		mux.HandleFunc("GET /sprint/", s.handleBoardPage)
		mux.Handle("GET /sprint", redirectTo("/sprint/"))
		mux.HandleFunc("GET /api/sprint", s.handleBoardOverview)
		mux.HandleFunc("GET /api/sprint/panel/{id}", s.handleBoardPanel)
		mux.HandleFunc("GET /api/sprint/story/{id}", s.handleBoardStory)
		// Runbooks: the kb pages of type runbook that this board's sprints
		// can cite — read-only, one list and one body (panel_runbooks.go).
		mux.HandleFunc("GET /api/sprint/runbooks", s.handleRunbooks)
		mux.HandleFunc("GET /api/sprint/runbook/{slug}", s.handleRunbook)
	}

	closers := []func() error{}
	for _, p := range s.panels {
		if !p.Available {
			continue
		}
		h, closer := s.panelHandler(p)
		if closer != nil {
			closers = append(closers, closer)
		}
		if h == nil {
			continue
		}
		// Mount from Path, not Name: they are allowed to differ (the terminal is
		// "terminal" at /term/), and deriving the route from the name instead
		// silently serves a 404 at the very link the tile renders.
		mount := strings.TrimSuffix(p.Path, "/")
		if mount == "" {
			continue
		}
		mux.Handle(mount+"/", coopauth.Mount(mount, h))
		mux.Handle(mount, coopauth.Mount(mount, h))
	}

	// Everything else is the start page, including its client-side routes.
	mux.HandleFunc("/", s.handleSPA)

	// otelhttp costs nothing without an exporter: the global provider is a no-op,
	// so this is span-shaped bookkeeping that never leaves the process.
	h := otelhttp.NewHandler(
		coopauth.StripSpoofedTrustHeaders(s.consoleGate(mux)),
		otelServiceName,
	)
	closer := func() error {
		var err error
		for _, c := range closers {
			if cerr := c(); cerr != nil && err == nil {
				err = cerr
			}
		}
		return err
	}
	return s, h, closer, nil
}

// panelHandler returns the handler for a panel plus any closer it needs, or nil
// when the console has nothing to serve for it.
func (s *server) panelHandler(p Panel) (http.Handler, func() error) {
	switch p.Name {
	case "terminal", "mb", "sprint", "inbox":
		// Served above, at the launcher's own root, not mounted here.
		return nil, nil
	case "files":
		h, closer, err := filesPanel(s.opts.Scope, s.opts.AllowWrite)
		if err != nil {
			// An unmountable panel must not take the whole launcher down: the
			// tile says why and everything else still works.
			slog.Error("files panel", "err", err)
			return nil, nil
		}
		return h, closer
	case "meet":
		// The room is MOUNTED, not proxied to a separate `meet serve`: one
		// engine, one lease, one transcript. The pass-through gate is required —
		// see meet.MountOptions.
		return meet.HandlerWith(s.opts.Ctx, meet.MountOptions{Gate: passthrough}), nil
	}
	if p.Mode == atlas.WebProxy && p.Port > 0 {
		return s.proxyTo(p), nil
	}
	return nil, nil
}

// proxyTo reverse-proxies a separately-supervised service, and says so plainly
// when it is not running — a stopped tile that renders a connection-refused
// stack trace teaches nothing.
func (s *server) proxyTo(p Panel) http.Handler {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(p.Port)}
	rp := httputil.NewSingleHostReverseProxy(target)

	// The app must be able to build correct absolute URLs and correct redirects
	// from BEHIND two hops (cloudbox -> outpost -> console -> app). coopauth.Mount
	// has already stamped X-Forwarded-Prefix; these are the other two thirds of
	// the contract app authors are told to read instead of Host/r.TLS.
	inner := rp.Director
	rp.Director = func(req *http.Request) {
		prefix := coopauth.BasePrefix(req)
		host, proto := forwardedHostProto(req)
		inner(req)
		if prefix != "" {
			req.Header.Set(coopauth.HdrForwardedPrefix, prefix)
		}
		if host != "" {
			req.Header.Set(coopauth.HdrForwardedHost, host)
		}
		if proto != "" {
			req.Header.Set(coopauth.HdrForwardedProto, proto)
		}
	}

	// Rewrite a root-relative Location into the mount. This is what outpost
	// already does for cooperative apps, and without it the `custom` auth tier
	// cannot work at all: an app answering a login POST with `302 /home` would
	// send the browser to the console's launcher instead of back into the app.
	//
	// Only root-relative values are touched — an absolute URL is the app's
	// deliberate choice (an external IdP, say) and rewriting it would break a
	// redirect that was already correct.
	rp.ModifyResponse = func(resp *http.Response) error {
		loc := resp.Header.Get("Location")
		if loc == "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
			return nil
		}
		if resp.Request != nil {
			resp.Header.Set("Location", coopauth.PrefixPath(resp.Request, loc))
		}
		return nil
	}
	// FlushInterval -1 streams: these panels carry SSE and WebSockets, and any
	// buffering turns a live view into a hang.
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		msg := p.Label + " is not running on 127.0.0.1:" + strconv.Itoa(p.Port) + ".\n"
		if hint := p.StartHint(); hint != "" {
			msg += "Start it with:  " + hint + "\n"
		}
		_, _ = w.Write([]byte(msg))
	}
	return rp
}

func redirectTo(path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, coopauth.PrefixPath(r, path), http.StatusFound)
	})
}

// forwardedHostProto reports the host and scheme as the BROWSER sees them,
// derived from coopauth.ExternalBase so the answer is identical to the one the
// console's own pages compute.
func forwardedHostProto(r *http.Request) (host, proto string) {
	base := coopauth.ExternalBase(r)
	if base == "" {
		return "", ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", ""
	}
	return u.Host, u.Scheme
}
