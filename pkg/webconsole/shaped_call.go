// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/shape/boundary"
)

// A shaped app's data is private: a unit reaches another app only through
// that app's interface, as the same principal. The interface is the owning
// app's own units — `app call APP UNIT` in Bash# runs APP's api/UNIT.md
// exactly as its route would, with APP's revision, APP's data and APP's
// boundary for that principal. The calling unit never learns where the
// owner keeps anything.
//
// The interpreter runs out of process, so it reaches the console through a
// call broker: a loopback-only listener the console starts on first use.
// Each unit run gets a fresh token bound to its principal; the token is the
// whole authority (a request cannot name another principal) and is revoked
// when the run ends.

// maxCallDepth bounds nested cross-app calls (a calls b calls a ...).
const maxCallDepth = 4

type callDepthKey struct{}

func callDepth(ctx context.Context) int {
	d, _ := ctx.Value(callDepthKey{}).(int)
	return d
}

type callGrant struct {
	principal string
	from      string
	depth     int
}

// callBroker is the console's cross-app call endpoint.
type callBroker struct {
	s      *server
	mu     sync.Mutex
	srv    *http.Server
	url    string
	tokens map[string]callGrant
}

// mint returns an endpoint for one unit run of app from as principal, and
// the revoke that ends it.
func (b *callBroker) mint(principal, from string, depth int) (*boundary.Endpoint, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		b.srv = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second}
		b.url = "http://" + ln.Addr().String() + "/call"
		b.tokens = map[string]callGrant{}
		go func(srv *http.Server) { _ = srv.Serve(ln) }(b.srv)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	tok := hex.EncodeToString(raw)
	b.tokens[tok] = callGrant{principal: principal, from: from, depth: depth}
	return &boundary.Endpoint{URL: b.url, Token: tok}, func() {
		b.mu.Lock()
		delete(b.tokens, tok)
		b.mu.Unlock()
	}, nil
}

func (b *callBroker) close() error {
	b.mu.Lock()
	srv := b.srv
	b.srv = nil
	b.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

func (b *callBroker) grant(tok string) (callGrant, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.tokens[tok]
	return g, ok && tok != ""
}

// ServeHTTP answers POST /call/APP/UNIT[?QUERY] for a live token: 404 for
// an app or unit that does not exist (a normal answer, never a cascade),
// otherwise whatever the owning app's unit answers.
func (b *callBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g, ok := b.grant(r.Header.Get(boundary.CallTokenHeader))
	if !ok {
		http.Error(w, "no access", http.StatusUnauthorized)
		return
	}
	if g.depth >= maxCallDepth {
		http.Error(w, "cross-app call depth exceeded", http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/call/")
	app, unitName, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || app == "" || unitName == "" || strings.Contains(unitName, "/") {
		http.NotFound(w, r)
		return
	}
	var target *shapedApp
	for _, p := range b.s.currentPanels() {
		if p.Name == app && p.ShapedSource != "" && p.Available {
			target = b.s.shapedApp(p)
			break
		}
	}
	if target == nil {
		http.NotFound(w, r)
		return
	}
	repo, err := target.open()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := target.active(repo, g.principal)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tree, err := c.Tree()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dataDir, cleanup, err := target.dataDir(g.principal)
	if err != nil {
		http.Error(w, "app data unavailable", http.StatusServiceUnavailable)
		return
	}
	defer cleanup()
	ctx := context.WithValue(r.Context(), callDepthKey{}, g.depth+1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/"+unitName+"?"+r.URL.RawQuery, r.Body)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	target.serveAPI(w, req, tree, unitName, g.principal, dataDir)
}

// admit narrows what a unit declares outside its data to what principal
// granted this app at adoption (no principal grants nothing).
func (a *shapedApp) admit(principal string, declared boundary.Grants) (boundary.Grants, error) {
	if declared.Empty() {
		return boundary.Grants{}, nil
	}
	userHome, _ := os.UserHomeDir()
	declared = boundary.Grants{
		Read:     boundary.ExpandHome(declared.Read, userHome),
		Write:    boundary.ExpandHome(declared.Write, userHome),
		Programs: declared.Programs,
	}
	var granted boundary.Grants
	if principal != "" {
		st, err := a.store(principal)
		if err != nil {
			return boundary.Grants{}, err
		}
		state, err := st.Read()
		if err != nil {
			return boundary.Grants{}, err
		}
		if g := state.Granted; g != nil {
			granted = boundary.Grants{
				Read:     boundary.ExpandHome(g.Read, userHome),
				Write:    boundary.ExpandHome(g.Write, userHome),
				Programs: g.Programs,
			}
		}
	}
	return boundary.Admit(declared, granted)
}

// policy is one run's boundary: its own data, the bashy home and the
// shaping roots out of reach, the admitted grants, and — for a principal —
// a cross-app call endpoint, revoked by the returned func.
func (a *shapedApp) policy(ctx context.Context, principal, dataDir string, grants boundary.Grants) (boundary.Policy, func(), error) {
	home, err := a.opts.home()
	if err != nil {
		return boundary.Policy{}, nil, err
	}
	p := boundary.Policy{App: a.name, Principal: principal, Data: dataDir, Home: home, Grants: grants}
	if wr, err := a.opts.workRoot(); err == nil {
		p.Protected = append(p.Protected, wr)
	}
	if a.repo != "" {
		p.Protected = append(p.Protected, a.repo)
	}
	revoke := func() {}
	// Only a run on the principal's real data may call out: a preview runs
	// on a private copy, and a call would reach the other app's real data.
	if principal != "" && a.calls != nil && a.isLiveData(principal, dataDir) {
		ep, rv, err := a.calls.mint(principal, a.name, callDepth(ctx))
		if err != nil {
			return boundary.Policy{}, nil, errors.Join(errors.New("shaped app: cross-app call endpoint"), err)
		}
		p.Call, revoke = ep, rv
	}
	return p, revoke, nil
}

// isLiveData reports whether dataDir is principal's real app data (not a
// preview copy).
func (a *shapedApp) isLiveData(principal, dataDir string) bool {
	st, err := a.store(principal)
	if err != nil {
		return false
	}
	d, err := st.DataDir()
	return err == nil && filepath.Clean(d) == filepath.Clean(dataDir)
}
