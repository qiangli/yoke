// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/shape/appserver"
	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
)

// A shaped app may declare a long-running server of its own in shaped.json
// (pkg/shape/appserver). Its revisions are still just commits: the console
// builds the server from the requesting principal's active revision into a
// cache keyed by that revision's TREE (a restore commits forward to an
// identical tree and reuses the binary), starts it lazily on a loopback port
// it picks with the principal's data directory, proxies the request to it,
// and stops it once idle. Nothing about history, preview, restore or undo
// changes — they move which tree is active, and the next request follows.
//
// The server runs under the same guards as an api unit (pkg/shape/contain:
// scrubbed env, the read,write effect cap, procguard, cwd and HOME in its
// data directory), and it is reached only on the 127.0.0.1 address the
// console handed it. A compiled server is not interpreter-confined the way a
// Bash# unit is; an app that needs isolation is not served at sandbox level
// (that request refuses rather than downgrades).

const (
	defaultServerIdle   = 10 * time.Minute
	serverBuildTimeout  = 5 * time.Minute
	serverReadyTimeout  = 15 * time.Second
	serverBuildRetry    = time.Minute
	serverRunLimit      = 24 * time.Hour
	serverStderrTailLen = 4 << 10
)

// shapedServers is the console's pool of built binaries and running shaped
// app servers. One per console; its closer stops every server.
type shapedServers struct {
	mu     sync.Mutex
	builds map[string]*serverBuild
	procs  map[string]*serverProc
	reaper bool
	closed bool
	stop   chan struct{}
}

type serverBuild struct {
	done chan struct{}
	bin  string
	err  error
	at   time.Time
}

type serverProc struct {
	key     string
	addr    string
	ready   chan struct{}
	err     error // set before ready closes when the start failed
	exited  chan struct{}
	cancel  context.CancelFunc
	scratch string // removed on stop: an anonymous caller's data directory
	stderr  *tailBuffer

	mu       sync.Mutex
	last     time.Time
	inflight int
}

func (p *serverProc) touch(delta int) {
	p.mu.Lock()
	p.inflight += delta
	p.last = time.Now()
	p.mu.Unlock()
}

func (p *serverProc) idleSince(cut time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight == 0 && p.last.Before(cut)
}

// serverManifest reads shaped.json from a revision. ok is false for an app
// without one (or without a server in it): it is served as before.
func serverManifest(tree *object.Tree) (*appserver.Server, bool, error) {
	f, err := tree.File(appserver.File)
	if err != nil {
		return nil, false, nil
	}
	body, err := f.Contents()
	if err != nil {
		return nil, false, err
	}
	m, err := appserver.Parse([]byte(body))
	if err != nil {
		return nil, false, err
	}
	if m.Server == nil {
		return nil, false, nil
	}
	return m.Server, true, nil
}

func (o ShapeOptions) goTool() string {
	if o.GoTool != "" {
		return o.GoTool
	}
	return "go"
}

func (o ShapeOptions) serverIdle() time.Duration {
	if o.ServerIdle > 0 {
		return o.ServerIdle
	}
	return defaultServerIdle
}

// serveServer answers one request from the server commit c declares.
// dataDir is the principal's data directory; empty (no principal) gives the
// server a scratch directory of its own that is removed when it stops.
func (a *shapedApp) serveServer(w http.ResponseWriter, r *http.Request, c *object.Commit, srv *appserver.Server, principal, dataDir string) {
	if a.servers == nil {
		http.Error(w, "shaped app servers are not available on this console", http.StatusServiceUnavailable)
		return
	}
	if a.opts.Level == contain.LevelSandbox {
		http.Error(w, "shaped app "+a.name+" declares a server, which cannot run at sandbox level", http.StatusServiceUnavailable)
		return
	}
	bin, err := a.servers.build(r.Context(), a, c)
	if err != nil {
		http.Error(w, "shaped app "+a.name+": server build failed for revision "+short(c.Hash.String())+":\n"+err.Error(), http.StatusInternalServerError)
		return
	}
	p, err := a.servers.start(a, c.TreeHash.String(), bin, srv, principal, dataDir)
	if err != nil {
		http.Error(w, "shaped app "+a.name+": server failed to start:\n"+err.Error(), http.StatusBadGateway)
		return
	}
	defer p.touch(-1)
	shapedServerProxy(p).ServeHTTP(w, r)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// build returns the cached binary for c's tree, building it once. A failed
// build is remembered briefly so a page's asset burst does not rebuild, and
// it is never answered with another tree's binary.
func (s *shapedServers) build(ctx context.Context, a *shapedApp, c *object.Commit) (string, error) {
	key := a.name + "\x00" + c.TreeHash.String()
	s.mu.Lock()
	if s.builds == nil {
		s.builds = map[string]*serverBuild{}
	}
	b, ok := s.builds[key]
	if ok {
		select {
		case <-b.done:
			if b.err != nil && time.Since(b.at) > serverBuildRetry {
				ok = false
			}
		default:
		}
	}
	if !ok {
		b = &serverBuild{done: make(chan struct{})}
		s.builds[key] = b
		s.mu.Unlock()
		b.bin, b.err = a.buildServer(c)
		b.at = time.Now()
		close(b.done)
	} else {
		s.mu.Unlock()
	}
	select {
	case <-b.done:
		return b.bin, b.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// buildServer extracts c's tree and runs the fixed build. The binary lives
// at <home>/shape-cache/<app>/<tree>/server, so a console restart reuses it.
func (a *shapedApp) buildServer(c *object.Commit) (string, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	srv, ok, err := serverManifest(tree)
	if err != nil || !ok {
		return "", fmt.Errorf("no server manifest: %v", err)
	}
	home, err := a.opts.home()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "shape-cache", a.name, c.TreeHash.String())
	bin := filepath.Join(dir, "server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() {
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(dir, "build-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	if err := extractTree(tree, src); err != nil {
		return "", fmt.Errorf("extract revision: %w", err)
	}
	out := filepath.Join(work, filepath.Base(bin))
	ctx, cancel := context.WithTimeout(context.Background(), serverBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.opts.goTool(), "build", "-trimpath", "-o", out, "./"+srv.Main)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(strings.ReplaceAll(log.String(), src+string(filepath.Separator), ""))
		return "", fmt.Errorf("go build ./%s: %v\n%s", srv.Main, err, msg)
	}
	if err := os.Rename(out, bin); err != nil {
		return "", err
	}
	return bin, nil
}

// extractTree writes a revision's regular files under dst. Symlinks and
// submodules are skipped: a link could point anywhere on the host.
func extractTree(tree *object.Tree, dst string) error {
	return tree.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable && f.Mode != filemode.Deprecated {
			return nil
		}
		rel := path.Clean(f.Name)
		if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path %q", f.Name)
		}
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rd, err := f.Reader()
		if err != nil {
			return err
		}
		defer rd.Close()
		mode := os.FileMode(0o644)
		if f.Mode == filemode.Executable {
			mode = 0o755
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(out, rd)
		if err := out.Close(); cerr == nil {
			cerr = err
		}
		return cerr
	})
}

// start returns the running server for (app, tree, data), starting it if
// needed and waiting until it accepts connections. The returned server
// counts one request in flight — taken under the pool lock, so the idle
// sweep cannot stop it between lookup and use; the caller releases it with
// touch(-1).
func (s *shapedServers) start(a *shapedApp, tree, bin string, srv *appserver.Server, principal, dataDir string) (*serverProc, error) {
	key := a.name + "\x00" + tree + "\x00" + principal + "\x00" + dataDir
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("console is shutting down")
	}
	if s.procs == nil {
		s.procs = map[string]*serverProc{}
	}
	p, ok := s.procs[key]
	if ok {
		select {
		case <-p.exited:
			delete(s.procs, key)
			ok = false
		default:
		}
	}
	if ok {
		p.touch(1)
		s.mu.Unlock()
		<-p.ready
		if p.err != nil {
			return nil, p.err
		}
		return p, nil
	}
	p = &serverProc{key: key, ready: make(chan struct{}), exited: make(chan struct{}),
		stderr: &tailBuffer{max: serverStderrTailLen}, last: time.Now(), inflight: 1}
	s.procs[key] = p
	s.startReaper(a.opts.serverIdle())
	s.mu.Unlock()

	p.err = s.launch(a, p, bin, srv, principal, dataDir)
	if p.err != nil {
		s.mu.Lock()
		if s.procs[key] == p {
			delete(s.procs, key)
		}
		s.mu.Unlock()
	}
	close(p.ready)
	return p, p.err
}

func (s *shapedServers) launch(a *shapedApp, p *serverProc, bin string, srv *appserver.Server, principal, dataDir string) error {
	if dataDir == "" {
		d, err := os.MkdirTemp("", "shape-anon-*")
		if err != nil {
			return err
		}
		p.scratch, dataDir = d, d
	}
	port, err := freeLoopbackPort()
	if err != nil {
		p.cleanup()
		return err
	}
	p.addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	pr, pw, err := os.Pipe()
	if err != nil {
		p.cleanup()
		return err
	}
	go func() { _, _ = io.Copy(p.stderr, pr); _ = pr.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	argv := append([]string{bin}, srv.Argv(p.addr, port, dataDir)...)
	go func() {
		defer close(p.exited)
		defer p.cleanup()
		_, err := contain.Run(ctx, contain.Spec{
			Dir: dataDir, Argv: argv, Level: contain.LevelProcess, Cap: "read,write",
			Effects: []string{"read", "write"}, Timeout: serverRunLimit,
			EnvSet: map[string]string{
				shape.AppDataEnv: dataDir, "BASHY_APP": a.name, "BASHY_PRINCIPAL": principal,
				"BASHY_APP_ADDR": p.addr,
			},
			Stdout: pw, Stderr: pw,
		})
		_ = pw.Close()
		if err != nil {
			_, _ = fmt.Fprintf(p.stderr, "\n%v\n", err)
		}
	}()
	deadline := time.Now().Add(serverReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			return fmt.Errorf("server exited before it listened on %s:\n%s", p.addr, strings.TrimSpace(p.stderr.String()))
		default:
		}
		if c, err := net.DialTimeout("tcp", p.addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-p.exited
	return fmt.Errorf("server did not listen on %s within %s:\n%s", p.addr, serverReadyTimeout, strings.TrimSpace(p.stderr.String()))
}

func (p *serverProc) cleanup() {
	if p.scratch != "" {
		_ = os.RemoveAll(p.scratch)
	}
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// startReaper runs the idle sweep once per pool. Called with s.mu held.
func (s *shapedServers) startReaper(idle time.Duration) {
	if s.reaper {
		return
	}
	s.reaper = true
	s.stop = make(chan struct{})
	tick := idle / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	go func(stop <-chan struct{}) {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.reap(time.Now().Add(-idle))
			}
		}
	}(s.stop)
}

func (s *shapedServers) reap(cut time.Time) {
	var victims []*serverProc
	s.mu.Lock()
	for k, p := range s.procs {
		select {
		case <-p.ready:
		default:
			continue
		}
		exited := false
		select {
		case <-p.exited:
			exited = true
		default:
		}
		if exited || p.idleSince(cut) {
			delete(s.procs, k)
			victims = append(victims, p)
		}
	}
	s.mu.Unlock()
	for _, p := range victims {
		p.halt()
	}
}

func (p *serverProc) halt() {
	if p.cancel != nil {
		p.cancel()
	}
	<-p.exited
}

// running reports how many servers are up: tests watch the idle stop.
func (s *shapedServers) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs)
}

// close stops every server; later starts refuse.
func (s *shapedServers) close() error {
	s.mu.Lock()
	s.closed = true
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	procs := s.procs
	s.procs = nil
	s.mu.Unlock()
	for _, p := range procs {
		<-p.ready
		if p.err == nil {
			p.halt()
		}
	}
	return nil
}

// shapedServerProxy forwards one request, already stripped of the app's
// mount, to the server. The response keeps serveTree's rules: per-principal
// content is never cacheable, and the console's own session cookie never
// reaches app code.
func shapedServerProxy(p *serverProc) http.Handler {
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: p.addr})
	inner := rp.Director
	rp.Director = func(req *http.Request) {
		inner(req)
		req.Header.Del(PrincipalHeader)
		dropCookie(req, sessionCookie)
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Cache-Control")
		resp.Header.Del("X-Content-Type-Options")
		if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && resp.Request != nil {
			resp.Header.Set("Location", coopauth.PrefixPath(resp.Request, loc))
		}
		return nil
	}
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "shaped app server unreachable: "+err.Error(), http.StatusBadGateway)
	}
	return rp
}

func dropCookie(r *http.Request, name string) {
	cs := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cs {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

// tailBuffer keeps the last max bytes written: a server's stderr, quoted
// when it fails to start.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
