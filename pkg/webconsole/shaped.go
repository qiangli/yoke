// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/yoke/pkg/shape/selfbin"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	_ "github.com/qiangli/yoke/git" // pure-Go file transport for local clones
	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/unit"
)

// A shaped app is a versioned source tree the console serves itself: its
// public/ assets come from the requesting principal's active revision (the
// base revision when the principal has none, or there is no principal at
// all), and each api/<name>.md is a Bash# unit run once per request under
// bashy's guards with that principal's private data directory. There is no
// long-lived process per version — a revision is just a commit.

// ShapeOptions configures shaped apps and the Shape panel. The zero value is
// the production configuration minus the shaping seams: without a Delegator
// the panel refuses to start a candidate, without a Verifier nothing can be
// approved.
type ShapeOptions struct {
	// Home is the Bashy home holding per-principal state. Empty = shape.Home().
	Home string
	// WorkRoot holds candidate workspaces. Empty = <Home>/shape-work. It is
	// deliberately outside every principal's private directory: the shaping
	// agent works there and must not find user data beside its checkout.
	WorkRoot string
	// Runner is the argv prefix that runs a Bash# body given as `-c BODY`.
	// Empty = {"bashy"}.
	Runner []string
	// Level is the containment for api units and previews. Empty = process:
	// bashy's guards (scrubbed env, effect cap, timeout, limits), which
	// every OS provides — shaping never requires a sandbox. A unit that
	// declares @sandbox() or a bare @sandbox line runs at
	// contain.LevelSandbox through bashy oci whatever Level says, and
	// setting LevelSandbox here routes every unit there. A sandbox bashy
	// oci cannot provide refuses (never downgrades).
	Level contain.Level
	// OCI routes sandbox runs; nil = contain.BashyOCI{} (`bashy oci run`).
	OCI contain.OCIRunner
	// APITimeout bounds one api unit run. Zero = 30s.
	APITimeout time.Duration

	Delegate  Delegator
	Verifier  Verifier
	Activator Activator
}

func (o ShapeOptions) home() (string, error) {
	if o.Home != "" {
		return filepath.Abs(o.Home)
	}
	return shape.Home()
}

func (o ShapeOptions) workRoot() (string, error) {
	if o.WorkRoot != "" {
		return filepath.Abs(o.WorkRoot)
	}
	h, err := o.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "shape-work"), nil
}

func (o ShapeOptions) runner() []string {
	if len(o.Runner) > 0 {
		return o.Runner
	}
	return []string{selfbin.Resolve("")}
}

// shapedApp is one shaped panel bound to its checkout.
type shapedApp struct {
	name string
	repo string
	opts ShapeOptions
}

func (s *server) shapedApp(p Panel) *shapedApp {
	return &shapedApp{name: p.Name, repo: p.ShapedSource, opts: s.opts.Shape}
}

// store opens the principal's private state. principal must be non-empty.
func (a *shapedApp) store(principal string) (*shape.Store, error) {
	home, err := a.opts.home()
	if err != nil {
		return nil, err
	}
	return shape.OpenAt(home, a.name, principal)
}

func (a *shapedApp) open() (*gogit.Repository, error) {
	r, err := gogit.PlainOpen(a.repo)
	if err != nil {
		return nil, fmt.Errorf("shaped app %s: open source: %w", a.name, err)
	}
	return r, nil
}

// base is the app's shared revision: the checkout's HEAD.
func base(r *gogit.Repository) (*object.Commit, error) {
	ref, err := r.Head()
	if err != nil {
		return nil, err
	}
	return r.CommitObject(ref.Hash())
}

func resolveCommit(r *gogit.Repository, rev string) (*object.Commit, error) {
	h, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, err
	}
	return r.CommitObject(*h)
}

// active resolves the revision a request is served from. No principal means
// the base app, full stop. A recorded revision the checkout cannot resolve
// falls back to last-known-good, then to base — never to another principal's.
func (a *shapedApp) active(r *gogit.Repository, principal string) (*object.Commit, error) {
	b, err := base(r)
	if err != nil || principal == "" {
		return b, err
	}
	st, err := a.store(principal)
	if err != nil {
		return b, nil
	}
	state, err := st.Read()
	if err != nil {
		slog.Warn("shaped app: unreadable state, serving base", "app", a.name, "err", err)
		return b, nil
	}
	for _, rev := range []string{state.ActiveRevision, state.LastKnownGood} {
		if rev == "" {
			continue
		}
		if c, err := resolveCommit(r, rev); err == nil {
			return c, nil
		}
	}
	return b, nil
}

// handler serves the app at its mount for the request's principal.
func (a *shapedApp) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, err := a.open()
		if err != nil {
			http.Error(w, "shaped app source unavailable", http.StatusServiceUnavailable)
			return
		}
		principal, _ := PrincipalFromRequest(r)
		c, err := a.active(repo, principal)
		if err != nil {
			http.Error(w, "shaped app has no base revision", http.StatusServiceUnavailable)
			return
		}
		dataDir, cleanup, err := a.dataDir(principal)
		if err != nil {
			http.Error(w, "shaped app data unavailable", http.StatusServiceUnavailable)
			return
		}
		defer cleanup()
		a.serveTree(w, r, c, mountRel(r), principal, dataDir)
	})
}

// dataDir is the principal's BASHY_APP_DATA. Without a principal the unit gets
// a fresh scratch directory that is removed after the request: base behavior,
// no persistence, and no way to reach anyone's data.
func (a *shapedApp) dataDir(principal string) (string, func(), error) {
	if principal == "" {
		d, err := os.MkdirTemp("", "shape-anon-*")
		if err != nil {
			return "", nil, err
		}
		return d, func() { _ = os.RemoveAll(d) }, nil
	}
	st, err := a.store(principal)
	if err != nil {
		return "", nil, err
	}
	d, err := st.DataDir()
	return d, func() {}, err
}

// mountRel is the request path below the app's mount, set by coopauth.Mount
// (which strips the prefix) or left whole by an un-mounted caller.
func mountRel(r *http.Request) string {
	return strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
}

// serveTree answers one request from commit c: api/<name> runs a unit, every
// other path is a public/ asset.
func (a *shapedApp) serveTree(w http.ResponseWriter, r *http.Request, c *object.Commit, rel, principal, dataDir string) {
	// Per-principal content behind one URL: no shared cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	tree, err := c.Tree()
	if err != nil {
		http.Error(w, "revision unreadable", http.StatusInternalServerError)
		return
	}
	if name, ok := strings.CutPrefix(rel, "api/"); ok {
		a.serveAPI(w, r, tree, name, principal, dataDir)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rel == "" || strings.HasSuffix(r.URL.Path, "/") {
		rel = path.Join(rel, "index.html")
	}
	f, err := tree.File("public/" + rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := f.Contents()
	if err != nil {
		http.Error(w, "asset unreadable", http.StatusInternalServerError)
		return
	}
	ct := mime.TypeByExtension(path.Ext(rel))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}

const (
	apiBodyLimit   = 64 << 10
	apiOutputLimit = 1 << 20
)

// serveAPI runs api/<name>.md — a Bash# unit — once, contained, with the
// request in CGI-style environment variables and the response on stdout.
func (a *shapedApp) serveAPI(w http.ResponseWriter, r *http.Request, tree *object.Tree, name, principal, dataDir string) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	f, err := tree.File("api/" + name + ".md")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	doc, err := f.Contents()
	if err != nil {
		http.Error(w, "unit unreadable", http.StatusInternalServerError)
		return
	}
	body := unit.BashSharpBody(doc)
	if body == "" {
		http.NotFound(w, r)
		return
	}
	// The cap is the shaped-app ceiling; the effects are what the unit's
	// contract declares (an uncontracted unit is taken at the ceiling). A
	// unit declaring more — network, exec, credentials — is refused before
	// it starts. Isolation is the unit's own request: @sandbox() — or a
	// bare @sandbox line — routes it to bashy oci; the app never sees how
	// the host provides it.
	effects := []string{"read", "write"}
	level := a.opts.Level
	if c, cerr := unit.ReadContract(body); cerr != nil {
		// Fail closed: an unreadable contract whose text names @sandbox
		// may be asking for isolation we cannot see; never run it
		// unsandboxed.
		if mentionsSandboxRequest(body) {
			http.Error(w, "unit "+name+" may ask for a sandbox but its contract is unreadable: "+cerr.Error(), http.StatusServiceUnavailable)
			return
		}
	} else {
		if len(c.EffectCap()) > 0 {
			effects = c.EffectCap()
		}
		if c.Sandbox {
			level = contain.LevelSandbox
		}
	}
	reqBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, apiBodyLimit))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	out, res, err := a.runUnit(r.Context(), dataDir, body, name, level, effects, map[string]string{
		"REQUEST_METHOD":     r.Method,
		"QUERY_STRING":       r.URL.RawQuery,
		"CONTENT_TYPE":       r.Header.Get("Content-Type"),
		"BASHY_REQUEST_BODY": string(reqBody),
		"BASHY_APP":          a.name,
		"BASHY_PRINCIPAL":    principal,
	})
	if err != nil {
		var ee *contain.EffectError
		if errors.As(err, &ee) {
			http.Error(w, "unit "+name+" declares effects this app may not have: "+strings.Join(ee.Exceeded, ","), http.StatusForbidden)
			return
		}
		if errors.Is(err, contain.ErrUnavailable) {
			http.Error(w, "unit "+name+" could not run as it asked (a requested sandbox comes from bashy oci): "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		slog.Error("shaped app unit", "app", a.name, "unit", name, "err", err)
		http.Error(w, "unit failed to run", http.StatusInternalServerError)
		return
	}
	if res.ExitCode != 0 {
		http.Error(w, fmt.Sprintf("unit %s failed (exit %d)", name, res.ExitCode), http.StatusInternalServerError)
		return
	}
	if json.Valid(out) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	_, _ = w.Write(out)
}

// mentionsSandboxRequest reports whether body has a decorator-position
// @sandbox line: the first non-blank characters name @sandbox, followed by
// end of line, parens or whitespace. Quoted prose (echo '@sandbox') never
// matches; a heredoc line could, but that only matters for a body whose
// contract already failed to parse — refusing a broken unit is safe.
func mentionsSandboxRequest(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(line, "@sandbox") {
			continue
		}
		rest := line[len("@sandbox"):]
		if rest == "" || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\r' {
			return true
		}
	}
	return false
}

// runUnit runs one Bash# body under pkg/shape/contain with dataDir as its
// cwd and its BASHY_APP_DATA (and, in a sandbox, its only mount).
func (a *shapedApp) runUnit(ctx context.Context, dataDir, body, name string, level contain.Level, effects []string, env map[string]string) ([]byte, contain.Result, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, contain.Result{}, err
	}
	defer pr.Close()
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(pr, apiOutputLimit))
		_, _ = io.Copy(io.Discard, pr)
		got <- b
	}()
	set := map[string]string{shape.AppDataEnv: dataDir}
	for k, v := range env {
		set[k] = v
	}
	timeout := a.opts.APITimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	argv := append(append([]string(nil), a.opts.runner()...), "-c", body, "api/"+name)
	res, runErr := contain.Run(ctx, contain.Spec{
		Dir: dataDir, Argv: argv, Level: level, OCI: a.opts.OCI, Cap: "read,write", Effects: effects,
		EnvSet: set, Timeout: timeout, Stdout: pw,
	})
	_ = pw.Close()
	out := <-got
	return out, res, runErr
}
