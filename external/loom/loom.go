// Package loom runs Gitea as a managed external binary (pkg/binmgr) — the git
// forge for the dhnt mesh. The Gitea binary is downloaded → sha256-verified →
// cached by binmgr, never compiled in; bashy ("the OS of binaries") launches it
// via `bashy loom`, and outpost exposes it over the mesh. "loom" keeps ycode's
// name for the gitea-backed forge. See dhnt/docs/external-binary-builtins.md.
package loom

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/coopauth"
)

const (
	// DefaultVersion pins the Gitea release loom runs. "" / "latest" resolves the
	// newest release dynamically; pin for reproducibility ($LOOM_GITEA_VERSION
	// or --gitea-version override). go-gitea/gitea is MIT.
	DefaultVersion   = "latest"
	DefaultAddr      = "127.0.0.1"
	DefaultPort      = 31880
	DefaultProxyPort = 31881
	LoopbackUser     = "admin"
	LoopbackPassword = "admin"
	LoopbackEmail    = "admin@localhost"
	LoopbackName     = "admin"
)

// Spec is the binmgr GitHub spec loom resolves the Gitea binary from.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{Name: "loom", Repo: "go-gitea/gitea", Version: version}
}

// DefaultDataDir is loom's work-path (Gitea data + config + repos).
func DefaultDataDir() string {
	if d := os.Getenv("LOOM_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "loom")
	}
	return filepath.Join(home, ".agents", "bashy", "loom")
}

// Options configures Serve.
type Options struct {
	Version   string // Gitea release (default DefaultVersion)
	DataDir   string // work-path (default DefaultDataDir)
	Addr      string // HTTP bind addr (default 127.0.0.1 — loopback for the mesh)
	Port      int    // HTTP port (default 31880)
	ProxyPort int    // HTTP reverse-proxy port (default 31881)
	RootURL   string // Gitea public/UI URL (default http://127.0.0.1:<port>/)
	// Actions toggles Gitea's built-in GitHub-Actions-compatible CI. nil = the
	// default (on, unless $LOOM_ACTIONS is "0"/"false"). This is what makes loom
	// a local CI control plane: act_runner registers against it and dials out
	// over the mesh. See dhnt/docs/local-p2p-cicd.md.
	Actions *bool
	// Owner is the host owner's identity (email or handle) — ALWAYS made a loom
	// site-admin. The operator who registered the host must own its forge. Empty
	// falls back to the OS user running loom (plus $LOOM_OWNER). A caller that
	// knows the paired cloud account (outpost) passes that email here.
	Owner  string
	Stdout io.Writer
	Stderr io.Writer
}

// actionsOn resolves the effective Actions toggle: an explicit Options.Actions
// wins; otherwise on unless $LOOM_ACTIONS is "0"/"false"/"no"/"off".
func (o *Options) actionsOn() bool {
	if o.Actions != nil {
		return *o.Actions
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOOM_ACTIONS"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func (o *Options) defaults() {
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	if o.DataDir == "" {
		o.DataDir = DefaultDataDir()
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.ProxyPort == 0 {
		o.ProxyPort = DefaultProxyPort
	}
	if o.RootURL == "" {
		o.RootURL = os.Getenv("LOOM_ROOT_URL")
	}
	if o.RootURL == "" {
		o.RootURL = fmt.Sprintf("http://127.0.0.1:%d/", o.Port)
	}
	if !strings.HasSuffix(o.RootURL, "/") {
		o.RootURL += "/"
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
}

// Instance is a running loom (Gitea) server.
type Instance struct {
	URL     string // http://addr:port it serves on
	Addr    string // addr:port (for `mesh service add git <addr>`)
	Version string // resolved Gitea version
	proc    *binmgr.Process
}

type State struct {
	PID       int       `json:"pid"`
	URL       string    `json:"url"`
	ProxyPID  int       `json:"proxy_pid,omitempty"`
	ProxyURL  string    `json:"proxy_url,omitempty"`
	ProxyAddr string    `json:"proxy_addr,omitempty"`
	RootURL   string    `json:"root_url"`
	Addr      string    `json:"addr"`
	Version   string    `json:"version"`
	DataDir   string    `json:"data_dir"`
	LogPath   string    `json:"log_path"`
	StartedAt time.Time `json:"started_at"`
}

// Stop terminates the Gitea process gracefully.
func (i *Instance) Stop() error {
	if i == nil || i.proc == nil {
		return nil
	}
	return i.proc.Stop(10 * time.Second)
}

// Start resolves + launches `gitea web` bound to a loopback port (NON-blocking):
// it returns once the server answers (or errors). The config is seeded on first
// run (INSTALL_LOCK, SQLite, a generated SECRET_KEY) so it comes up ready, not on
// the /install screen. The caller owns the returned Instance's lifecycle — this
// is what outpost's wrap-harness builtin supervises and exposes over the mesh.
func Start(ctx context.Context, o Options) (*Instance, error) {
	o.defaults()
	tool, err := binmgr.ResolveGitHub(ctx, Spec(o.Version))
	if err != nil {
		return nil, fmt.Errorf("loom: resolve gitea: %w", err)
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return nil, fmt.Errorf("loom: fetch gitea: %w", err)
	}
	cfg, err := ensureConfig(o.DataDir, o.Addr, o.Port, o.RootURL, o.actionsOn())
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", o.Addr, o.Port)
	url := "http://" + addr
	proc, err := binmgr.Launch(ctx, bin, binmgr.RunSpec{
		Args:       []string{"web", "--config", cfg},
		Env:        []string{"GITEA_WORK_DIR=" + o.DataDir},
		Dir:        o.DataDir,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		HealthURL:  url,
		HealthWait: 60 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("loom: start gitea: %w", err)
	}
	if err := ensureLoopbackAdmin(ctx, bin, cfg, o.DataDir); err != nil {
		_ = proc.Stop(10 * time.Second)
		return nil, err
	}
	// Two admins are auto-added: the host OS user (local username) and any
	// explicit owner (the cloudbox-signup email the caller passes) — plus the
	// email allowlist file. All promoted (existing or new).
	ensureAdmins(ctx, bin, cfg, o.DataDir, url, append(loadAdmins(o.DataDir), ownerIdentities(o)...))
	return &Instance{URL: url, Addr: addr, Version: tool.Version, proc: proc}, nil
}

// Serve runs loom in the foreground, blocking until the context is cancelled or
// SIGINT/SIGTERM arrives (the `bashy loom serve` path).
func Serve(ctx context.Context, o Options) error {
	o.defaults()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	inst, err := Start(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Stdout, "loom (gitea %s) serving on %s — work dir %s\n", inst.Version, inst.URL, o.DataDir)
	fmt.Fprintln(o.Stdout, "expose it over the mesh:  outpost mesh service add git "+inst.Addr)

	// Auto-seed SDLC labels on every repo (created or migrated), continuously.
	go startLabelReconciler(ctx, inst.URL)

	<-ctx.Done()
	fmt.Fprintln(o.Stdout, "loom: shutting down…")
	return inst.Stop()
}

func statePath(dataDir string) string { return filepath.Join(dataDir, "loom-state.json") }

func logPath(dataDir string) string { return filepath.Join(dataDir, "loom.log") }

func readState(dataDir string) (State, error) {
	var st State
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func writeState(st State) error {
	if err := os.MkdirAll(st.DataDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(st.DataDir), append(data, '\n'), 0o600)
}

func removeState(dataDir string) error {
	err := os.Remove(statePath(dataDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func StartDaemon(ctx context.Context, o Options) (State, error) {
	o.defaults()
	if st, err := readState(o.DataDir); err == nil && healthy(ctx, st.URL, 2*time.Second) {
		proxyOK := st.ProxyURL == "" || healthy(ctx, st.ProxyURL, 2*time.Second)
		if st.RootURL == o.RootURL && st.Addr == fmt.Sprintf("%s:%d", o.Addr, o.Port) && proxyOK {
			return st, nil
		}
		_, _ = StopDaemon(o.DataDir, 10*time.Second)
	}
	tool, err := binmgr.ResolveGitHub(ctx, Spec(o.Version))
	if err != nil {
		return State{}, fmt.Errorf("loom: resolve gitea: %w", err)
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return State{}, fmt.Errorf("loom: fetch gitea: %w", err)
	}
	cfg, err := ensureConfig(o.DataDir, o.Addr, o.Port, o.RootURL, o.actionsOn())
	if err != nil {
		return State{}, err
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return State{}, err
	}
	logFile := logPath(o.DataDir)
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return State{}, err
	}
	cmd := exec.Command(bin, "web", "--config", cfg)
	cmd.Dir = o.DataDir
	cmd.Env = append(os.Environ(), "GITEA_WORK_DIR="+o.DataDir)
	cmd.Stdout = log
	cmd.Stderr = log
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return State{}, fmt.Errorf("loom: start gitea: %w", err)
	}
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
	}()
	addr := fmt.Sprintf("%s:%d", o.Addr, o.Port)
	proxyAddr := fmt.Sprintf("%s:%d", o.Addr, o.ProxyPort)
	proxyURL := "http://" + proxyAddr
	proxyArgs := []string{"loom", "proxy", "--target", "http://" + addr, "--addr", o.Addr, "--port", strconv.Itoa(o.ProxyPort)}
	if p := publicPrefix(o.RootURL); p != "" {
		proxyArgs = append(proxyArgs, "--public-prefix", p)
	}
	proxyCmd := exec.Command(os.Args[0], proxyArgs...)
	proxyCmd.Dir = o.DataDir
	proxyCmd.Stdout = log
	proxyCmd.Stderr = log
	setDetached(proxyCmd)
	if err := proxyCmd.Start(); err != nil {
		_ = cmd.Process.Kill()
		_ = log.Close()
		return State{}, fmt.Errorf("loom: start proxy: %w", err)
	}
	go func() {
		_ = proxyCmd.Wait()
	}()
	st := State{
		PID:       cmd.Process.Pid,
		URL:       "http://" + addr,
		ProxyPID:  proxyCmd.Process.Pid,
		ProxyURL:  proxyURL,
		ProxyAddr: proxyAddr,
		RootURL:   o.RootURL,
		Addr:      addr,
		Version:   tool.Version,
		DataDir:   o.DataDir,
		LogPath:   logFile,
		StartedAt: time.Now().UTC(),
	}
	if err := waitHTTP(ctx, st.URL, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = proxyCmd.Process.Kill()
		_ = removeState(o.DataDir)
		return State{}, err
	}
	if err := ensureLoopbackAdmin(ctx, bin, cfg, o.DataDir); err != nil {
		_ = cmd.Process.Kill()
		_ = proxyCmd.Process.Kill()
		_ = removeState(o.DataDir)
		return State{}, err
	}
	ensureAdmins(ctx, bin, cfg, o.DataDir, st.URL, append(loadAdmins(o.DataDir), ownerIdentities(o)...))
	if err := waitHTTP(ctx, st.ProxyURL, 30*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = proxyCmd.Process.Kill()
		_ = removeState(o.DataDir)
		return State{}, err
	}
	if err := writeState(st); err != nil {
		return State{}, err
	}
	return st, nil
}

func StopDaemon(dataDir string, timeout time.Duration) (State, error) {
	st, err := readState(dataDir)
	if err != nil {
		return st, err
	}
	proc, err := os.FindProcess(st.PID)
	if err != nil {
		return st, err
	}
	var proxyProc *os.Process
	if st.ProxyPID > 0 {
		proxyProc, _ = os.FindProcess(st.ProxyPID)
	}
	if proxyProc != nil {
		_ = proxyProc.Signal(os.Interrupt)
	}
	_ = proc.Signal(os.Interrupt)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proxyDead := st.ProxyURL == "" || !healthy(context.Background(), st.ProxyURL, 500*time.Millisecond)
		if !healthy(context.Background(), st.URL, 500*time.Millisecond) && proxyDead {
			_ = removeState(dataDir)
			return st, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	if proxyProc != nil {
		_ = proxyProc.Kill()
	}
	_ = proc.Kill()
	_ = removeState(dataDir)
	return st, nil
}

func ExposeService(ctx context.Context, service, addr string) error {
	service = strings.TrimSpace(service)
	addr = strings.TrimSpace(addr)
	if service == "" {
		service = "git"
	}
	if addr == "" {
		return fmt.Errorf("loom: no address to expose")
	}
	cmd := exec.CommandContext(ctx, "outpost", "mesh", "service", "add", service, addr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("loom: expose via outpost: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitHTTP(ctx context.Context, url string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		if healthy(ctx, url, 2*time.Second) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("loom: %s not healthy after %s", url, max)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func healthy(ctx context.Context, url string, timeout time.Duration) bool {
	client := http.Client{Timeout: timeout}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

func runProxy(ctx context.Context, addr string, port int, target, publicPrefix string, autoProvision bool) error {
	if addr == "" {
		addr = DefaultAddr
	}
	if port == 0 {
		port = DefaultProxyPort
	}
	if target == "" {
		target = fmt.Sprintf("http://127.0.0.1:%d", DefaultPort)
	}
	handler, err := loomProxyHandler(target, publicPrefix, autoProvision)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", addr, port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	// The proxy is the long-lived process in daemon mode — run the SDLC-label
	// reconciler here so repos created/migrated at runtime get auto-labelled.
	go startLabelReconciler(ctx, target)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func loomProxyHandler(target, publicPrefix string, autoProvision bool) (http.Handler, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("loom proxy: target: %w", err)
	}
	if targetURL.Scheme == "" || targetURL.Host == "" {
		return nil, fmt.Errorf("loom proxy: target must be absolute (got %q)", target)
	}
	rp := httputil.NewSingleHostReverseProxy(targetURL)
	prefix := cleanPublicPrefix(publicPrefix)
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		stripPublicPrefix(req.URL, prefix)
		baseDirector(req)
		req.Host = targetURL.Host
		if prefix != "" && req.Header.Get("X-Forwarded-Prefix") == "" {
			req.Header.Del("Accept-Encoding")
		}
		stripWebauthHeaders(req.Header)
		user, email, name := proxyIdentity(req)
		if user != "" {
			if name == "" {
				name = user
			}
			// EMAIL is the identity. Gitea's reverse-proxy auth matches an
			// EXISTING account by X-WEBAUTH-EMAIL when no username is sent
			// (getUserFromAuthEmail), so a vouched user is logged in with NO
			// username-collision surface — the email is unique. We send
			// X-WEBAUTH-USER — which is what triggers Gitea AUTO-REGISTRATION of
			// a brand-new account — only when auto-provisioning is on. With it
			// off (the default, a private forge), an unknown email stays
			// anonymous: it can still VIEW public repos, but only owner/admins
			// (pre-provisioned) and, when on, self-registered users can act. The
			// username is a readable handle only; identity is the email.
			req.Header.Set("X-WEBAUTH-EMAIL", firstNonEmpty(email, user))
			req.Header.Set("X-WEBAUTH-FULLNAME", name)
			if autoProvision {
				req.Header.Set("X-WEBAUTH-USER", coopauth.Username(user))
			}
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		if prefix == "" || resp == nil || resp.Request == nil || resp.Request.Header.Get("X-Forwarded-Prefix") != "" {
			return nil
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			if stripped := stripPathPrefix(loc, prefix); stripped != loc {
				resp.Header.Set("Location", stripped)
			}
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") || resp.Body == nil {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		body = rewriteLocalHTMLPrefix(body, prefix)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
	return rp, nil
}

func publicPrefix(rootURL string) string {
	u, err := url.Parse(rootURL)
	if err != nil {
		return ""
	}
	return cleanPublicPrefix(u.Path)
}

func cleanPublicPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func stripPublicPrefix(u *url.URL, prefix string) {
	if u == nil || prefix == "" {
		return
	}
	if u.Path == prefix {
		u.Path = "/"
		u.RawPath = ""
		return
	}
	if strings.HasPrefix(u.Path, prefix+"/") {
		u.Path = strings.TrimPrefix(u.Path, prefix)
		if u.Path == "" {
			u.Path = "/"
		}
		u.RawPath = ""
	}
}

func stripPathPrefix(pathOrURL, prefix string) string {
	if prefix == "" || pathOrURL == "" {
		return pathOrURL
	}
	if u, err := url.Parse(pathOrURL); err == nil && u.Scheme != "" {
		if stripped := stripPathPrefix(u.Path, prefix); stripped != u.Path {
			u.Path = stripped
			u.RawPath = ""
			return u.RequestURI()
		}
		return pathOrURL
	}
	if pathOrURL == prefix {
		return "/"
	}
	if strings.HasPrefix(pathOrURL, prefix+"/") {
		return strings.TrimPrefix(pathOrURL, prefix)
	}
	return pathOrURL
}

func rewriteLocalHTMLPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	body = bytes.ReplaceAll(body, []byte(`"`+prefix+`/`), []byte(`"/`))
	body = bytes.ReplaceAll(body, []byte(`'`+prefix+`/`), []byte(`'/`))
	body = bytes.ReplaceAll(body, []byte(`=`+prefix+`/`), []byte(`=/`))
	body = bytes.ReplaceAll(body, []byte(`:`+prefix+`/`), []byte(`:/`))
	body = bytes.ReplaceAll(body, []byte(prefix+`/`), []byte(`/`))
	return body
}

// autoProvisionEnv reports whether $LOOM_AUTO_PROVISION opts a private forge
// into auto-registering an account for any vouched user on first contact.
func autoProvisionEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOOM_AUTO_PROVISION"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func stripWebauthHeaders(h http.Header) {
	for _, k := range []string{"X-WEBAUTH-USER", "X-WEBAUTH-EMAIL", "X-WEBAUTH-FULLNAME"} {
		h.Del(k)
	}
}

func proxyIdentity(req *http.Request) (user, email, name string) {
	user = strings.TrimSpace(req.Header.Get("Remote-User"))
	email = strings.TrimSpace(req.Header.Get("Remote-Email"))
	name = strings.TrimSpace(req.Header.Get("Remote-Name"))
	if user != "" {
		return user, email, name
	}
	if !isLoopbackRemote(req.RemoteAddr) {
		return "", "", ""
	}
	return LoopbackUser, LoopbackEmail, LoopbackName
}

func ensureLoopbackAdmin(ctx context.Context, bin, cfg, dataDir string) error {
	args := []string{
		"--config", cfg,
		"--work-path", dataDir,
		"admin", "user", "create",
		"--username", LoopbackUser,
		"--password", LoopbackPassword,
		"--email", LoopbackEmail,
		"--fullname", LoopbackName,
		"--admin",
		"--must-change-password=false",
	}
	out, err := runGiteaAdmin(ctx, bin, dataDir, args...)
	if err == nil {
		return nil
	}
	msg := string(out)
	if !strings.Contains(strings.ToLower(msg), "already exists") {
		return fmt.Errorf("loom: ensure local admin: %w: %s", err, strings.TrimSpace(msg))
	}
	list, listErr := runGiteaAdmin(ctx, bin, dataDir, "--config", cfg, "--work-path", dataDir, "admin", "user", "list", "--admin")
	if listErr != nil {
		return fmt.Errorf("loom: check local admin: %w: %s", listErr, strings.TrimSpace(string(list)))
	}
	if !adminListContainsUser(string(list), LoopbackUser) {
		return fmt.Errorf("loom: local user %q already exists but is not an admin", LoopbackUser)
	}
	return nil
}

// loadAdmins reads the app admin allowlist at <dataDir>/admins — one email per
// line, '#' comments and blanks ignored. This is loom's OWN authority over who
// is admin (the coopauth model): the cloud tier is never trusted for admin, so
// an allowlisted email — or the loopback owner — administers the forge, everyone
// else is a regular user. Edit the file and restart loom to apply.
func loadAdmins(dataDir string) []string {
	b, err := os.ReadFile(filepath.Join(dataDir, "admins"))
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
			out = append(out, ln)
		}
	}
	return out
}

// ensureAdmins provisions each allowlisted email as a Gitea site-admin, keyed by
// coopauth.Username(email) — the SAME sanitized name the proxy stamps as
// X-WEBAUTH-USER — so when that user first arrives over SSO, reverse-proxy auth
// finds an existing ADMIN account instead of auto-registering a regular one.
// Best-effort per entry (loom must still boot); Gitea has no CLI to promote an
// already-registered regular user, so that edge is logged with the fix.
// ensureAdmins makes each identity a loom site-admin. An identity is an EMAIL
// (cloud SSO — the primary identity) OR a bare USERNAME (a GitHub/remote handle:
// the operator who registered a dev machine with cloudbox often acts remotely as
// their GitHub user, which is NOT the host OS user). Both are supported so the
// admin allowlist covers the real operator regardless of which identity the proxy
// stamps. baseURL is the loom API base for promoting already-existing accounts.
func ensureAdmins(ctx context.Context, bin, cfg, dataDir, baseURL string, identities []string) {
	seen := map[string]bool{}
	for _, id := range identities {
		if id = strings.TrimSpace(id); id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ensureUserAdmin(ctx, bin, cfg, dataDir, baseURL, id)
	}
}

// ensureUserAdmin makes one identity (email or bare username) a site-admin:
// pre-create it as admin (so a first proxy login matches an existing admin
// account instead of minting a non-admin shadow), or — if it already exists as a
// non-admin — promote it via the loopback-admin API (the gitea CLI can only
// create-as-admin, never promote). Best effort: logs an actionable warning, never
// blocks startup.
func ensureUserAdmin(ctx context.Context, bin, cfg, dataDir, baseURL, id string) {
	u := coopauth.Username(id)
	if u == "" || u == LoopbackUser {
		return
	}
	email := id
	if !strings.Contains(email, "@") {
		email = u + "@localhost" // a bare handle: synthesize a stable local email
	}
	out, err := runGiteaAdmin(ctx, bin, dataDir, "--config", cfg, "--work-path", dataDir,
		"admin", "user", "create", "--username", u, "--email", email,
		"--random-password", "--admin", "--must-change-password=false")
	if err == nil {
		slog.Info("loom: pre-provisioned site-admin", "user", u)
		return
	}
	if strings.Contains(strings.ToLower(string(out)), "already exists") {
		if perr := promoteAdminViaAPI(ctx, baseURL, u); perr != nil {
			slog.Warn("loom: admin exists but is not site-admin; auto-promote failed — promote in the loom UI (login as admin)", "user", u, "err", perr)
			return
		}
		slog.Info("loom: promoted existing user to site-admin", "user", u)
		return
	}
	slog.Warn("loom: ensure admin failed", "user", u, "err", err, "out", strings.TrimSpace(string(out)))
}

// ownerIdentities returns the host-owner identities to ALWAYS make site-admin,
// via the shared resolver coopauth.AdminIdentities (reused by every custom app):
// the LOCAL system-admin login (OS user — standalone, no cloudbox needed) plus,
// when supplied, an explicit owner (Options.Owner / $LOOM_OWNER, comma-separated,
// e.g. a cloudbox-signup email). loom-specific only in WHERE the explicit values
// come from; the resolution logic lives in coopauth.
func ownerIdentities(o Options) []string {
	return coopauth.AdminIdentities(o.Owner, os.Getenv("LOOM_OWNER"))
}

// promoteAdminViaAPI sets is_admin on an existing user via the Gitea admin API,
// authenticated as the loopback admin — the gitea CLI can only create-as-admin,
// not promote an existing user.
func promoteAdminViaAPI(ctx context.Context, baseURL, user string) error {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/admin/users/" + url.PathEscape(user)
	// Gitea's EditUser REQUIRES login_name + source_id (422 "[LoginName]: Required"
	// otherwise). loom users are local (source_id 0); login_name = the username.
	payload, _ := json.Marshal(map[string]any{"login_name": user, "source_id": 0, "admin": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.SetBasicAuth(LoopbackUser, LoopbackPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PATCH admin/users/%s: HTTP %d: %s", user, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func runGiteaAdmin(ctx context.Context, bin, dataDir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(), "GITEA_WORK_DIR="+dataDir)
	return cmd.CombinedOutput()
}

func adminListContainsUser(output, user string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			if field == user {
				return true
			}
		}
	}
	return false
}

// defaultRepoLabels are the SDLC labels every loom repo needs so the loom-driven
// SDLC loop works the moment a repo is CREATED or MIGRATED — no manual setup.
var defaultRepoLabels = []struct{ Name, Color, Desc string }{
	{"sdlc:go", "#0e8a16", "trigger the SDLC conductor to implement + publish this issue"},
	{"deploy:prod", "#b60205", "publish to prod"},
	{"deploy:preview", "#fbca04", "build a safe local preview"},
}

// startLabelReconciler ensures every repo carries the default SDLC labels, at
// startup and then on a timer — so a repo created OR migrated while loom is
// running is auto-labelled without manual setup ("trace new repos, seed labels").
// Standalone: uses the loopback-admin API, no cloudbox. Blocks until ctx is done,
// so callers run it in a goroutine from a long-lived process (Serve / the proxy).
func startLabelReconciler(ctx context.Context, baseURL string) {
	reconcile := func() {
		if err := reconcileRepoLabels(ctx, baseURL); err != nil {
			slog.Debug("loom: label reconcile", "err", err)
		}
	}
	reconcile()
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		}
	}
}

func reconcileRepoLabels(ctx context.Context, baseURL string) error {
	repos, err := listAllRepos(ctx, baseURL)
	if err != nil {
		return err
	}
	for _, full := range repos {
		have, err := repoLabelNames(ctx, baseURL, full)
		if err != nil {
			continue
		}
		for _, l := range defaultRepoLabels {
			if have[l.Name] {
				continue
			}
			if err := createRepoLabel(ctx, baseURL, full, l.Name, l.Color, l.Desc); err == nil {
				slog.Info("loom: seeded SDLC label", "repo", full, "label", l.Name)
			}
		}
	}
	return nil
}

// loomAdminReq builds a loopback-admin-authenticated request to the loom API.
func loomAdminReq(ctx context.Context, method, endpoint, body string) (*http.Request, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(LoopbackUser, LoopbackPassword)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// listAllRepos returns every repo's full_name (admin sees all), paginated.
func listAllRepos(ctx context.Context, baseURL string) ([]string, error) {
	base := strings.TrimRight(baseURL, "/")
	var out []string
	for page := 1; page <= 100; page++ {
		req, err := loomAdminReq(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/repos/search?limit=50&page=%d", base, page), "")
		if err != nil {
			return out, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return out, err
		}
		var body struct {
			Data []struct {
				FullName string `json:"full_name"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		for _, d := range body.Data {
			out = append(out, d.FullName)
		}
		if len(body.Data) < 50 {
			break
		}
	}
	return out, nil
}

func repoLabelNames(ctx context.Context, baseURL, full string) (map[string]bool, error) {
	req, err := loomAdminReq(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/repos/"+full+"/labels?limit=100", "")
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&labels); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(labels))
	for _, l := range labels {
		m[l.Name] = true
	}
	return m, nil
}

func createRepoLabel(ctx context.Context, baseURL, full, name, color, desc string) error {
	payload, _ := json.Marshal(map[string]string{"name": name, "color": color, "description": desc})
	req, err := loomAdminReq(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/repos/"+full+"/labels", string(payload))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create label %s on %s: HTTP %d", name, full, resp.StatusCode)
	}
	return nil
}

func isLoopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureConfig writes a minimal Gitea app.ini on first run and returns its path.
// On an existing app.ini it reuses it, but reconciles the server URL/listener and
// actions toggle so an already-initialized data dir can be exposed over the mesh
// without manual app.ini surgery.
func ensureConfig(dataDir, addr string, port int, rootURL string, actions bool) (string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	if err := ensureCustomUI(dataDir); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dataDir, "app.ini")
	if existing, err := os.ReadFile(cfgPath); err == nil {
		merged := ensureServerSection(string(existing), addr, port, rootURL)
		merged = ensureServiceSection(merged)
		merged = ensureActionsSection(merged, actions)
		if merged != string(existing) {
			if err := os.WriteFile(cfgPath, []byte(merged), 0o600); err != nil {
				return "", err
			}
		}
		return cfgPath, nil // already configured — reuse (with actions reconciled)
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", err
	}
	ini := renderConfig(dataDir, addr, port, rootURL, secret, actions)
	if err := os.WriteFile(cfgPath, []byte(ini), 0o600); err != nil {
		return "", err
	}
	return cfgPath, nil
}

func ensureCustomUI(dataDir string) error {
	headerPath := filepath.Join(dataDir, "custom", "templates", "custom", "header.tmpl")
	if err := os.MkdirAll(filepath.Dir(headerPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(headerPath, []byte(loomHeaderTemplate), 0o644)
}

const loomHeaderTemplate = `<style>
#navbar .navbar-left > a.item[target="_blank"][href="https://docs.gitea.com"] {
	display: none !important;
}

.page-footer {
	display: none !important;
}

.page-content.home .ui.stackable.middle.very.relaxed.page.grid .column > p.large {
	display: none !important;
}
</style>
<script>
document.addEventListener('DOMContentLoaded', () => {
	const logo = document.getElementById('navbar-logo');
	if (!logo) return;
	const appSubUrl = window.config && window.config.appSubUrl;
	if (appSubUrl) {
		logo.href = appSubUrl + '/';
		return;
	}
	const path = window.location.pathname;
	const appMount = '/app/loom/';
	const appAt = path.indexOf(appMount);
	if (appAt >= 0) {
		logo.href = path.slice(0, appAt + appMount.length);
		return;
	}
	if (path === '/loom' || path.startsWith('/loom/')) {
		logo.href = '/loom/';
		return;
	}
	logo.href = '/';
});
</script>
`

func ensureServerSection(ini, addr string, port int, rootURL string) string {
	updates := map[string]string{
		"HTTP_ADDR":   addr,
		"HTTP_PORT":   fmt.Sprintf("%d", port),
		"ROOT_URL":    rootURL,
		"DISABLE_SSH": "true",
	}
	return ensureSectionKeys(ini, "server", updates)
}

func ensureServiceSection(ini string) string {
	return ensureSectionKeys(ini, "service", map[string]string{
		"DISABLE_REGISTRATION":                   "true",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION":    "true",
		"ENABLE_REVERSE_PROXY_AUTO_REGISTRATION": "true",
		"ENABLE_REVERSE_PROXY_EMAIL":             "true",
		"ENABLE_REVERSE_PROXY_FULL_NAME":         "true",
		"REVERSE_PROXY_AUTHENTICATION_USER":      "X-WEBAUTH-USER",
		"REVERSE_PROXY_AUTHENTICATION_EMAIL":     "X-WEBAUTH-EMAIL",
		"REVERSE_PROXY_AUTHENTICATION_FULL_NAME": "X-WEBAUTH-FULLNAME",
		"REVERSE_PROXY_TRUSTED_PROXIES":          "127.0.0.0/8,::1/128",
	})
}

// ensureActionsSection reconciles the [actions] ENABLED key in an existing
// app.ini to want. It rewrites an existing key, or appends the section if absent.
// Minimal INI surgery — Gitea owns the rest of the file, we touch only this key.
func ensureActionsSection(ini string, want bool) string {
	val := "false"
	if want {
		val = "true"
	}
	return ensureSectionKeys(ini, "actions", map[string]string{"ENABLED": val})
}

func ensureSectionKeys(ini, section string, updates map[string]string) string {
	lines := strings.Split(ini, "\n")
	sectionHeader := "[" + section + "]"
	inSection := false
	seenSection := false
	seenKeys := map[string]bool{}
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			if inSection {
				lines = insertMissingSectionKeys(lines, i, updates, seenKeys)
				return strings.Join(lines, "\n")
			}
			inSection = strings.EqualFold(t, sectionHeader)
			if inSection {
				seenSection = true
			}
			continue
		}
		if inSection {
			k := strings.TrimSpace(strings.SplitN(t, "=", 2)[0])
			for wantKey, wantValue := range updates {
				if strings.EqualFold(k, wantKey) {
					lines[i] = wantKey + " = " + wantValue
					seenKeys[wantKey] = true
					break
				}
			}
		}
	}
	if inSection {
		lines = insertMissingSectionKeys(lines, len(lines), updates, seenKeys)
		return strings.Join(lines, "\n")
	}
	var b strings.Builder
	if strings.HasSuffix(ini, "\n") {
		b.WriteString(ini)
	} else {
		b.WriteString(ini)
		b.WriteString("\n")
	}
	if seenSection {
		return b.String()
	}
	fmt.Fprintf(&b, "[%s]\n", section)
	for key, value := range updates {
		fmt.Fprintf(&b, "%s = %s\n", key, value)
	}
	return b.String()
}

func insertMissingSectionKeys(lines []string, at int, updates map[string]string, seen map[string]bool) []string {
	var missing []string
	for key, value := range updates {
		if !seen[key] {
			missing = append(missing, key+" = "+value)
		}
	}
	if len(missing) == 0 {
		return lines
	}
	sort.Strings(missing)
	out := append([]string{}, lines[:at]...)
	out = append(out, missing...)
	out = append(out, lines[at:]...)
	return out
}

// renderConfig is the minimal app.ini: INSTALL_LOCK so it boots ready, SQLite so
// there's no external DB, loopback bind so only the mesh reaches it. Gitea
// auto-generates + persists INTERNAL_TOKEN on first run when it's absent.
func renderConfig(dataDir, addr string, port int, rootURL string, secret string, actions bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "APP_NAME = loom\nRUN_MODE = prod\n\n")
	fmt.Fprintf(&b, "[server]\nHTTP_ADDR = %s\nHTTP_PORT = %d\nROOT_URL = %s\nDISABLE_SSH = true\n\n", addr, port, rootURL)
	fmt.Fprintf(&b, "[database]\nDB_TYPE = sqlite3\nPATH = %s\n\n", filepath.Join(dataDir, "gitea.db"))
	fmt.Fprintf(&b, "[repository]\nROOT = %s\n\n", filepath.Join(dataDir, "repositories"))
	fmt.Fprintf(&b, "[security]\nINSTALL_LOCK = true\nSECRET_KEY = %s\n\n", secret)
	fmt.Fprintf(&b, "[service]\n")
	for _, key := range []string{
		"DISABLE_REGISTRATION",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION",
		"ENABLE_REVERSE_PROXY_AUTO_REGISTRATION",
		"ENABLE_REVERSE_PROXY_EMAIL",
		"ENABLE_REVERSE_PROXY_FULL_NAME",
		"REVERSE_PROXY_AUTHENTICATION_USER",
		"REVERSE_PROXY_AUTHENTICATION_EMAIL",
		"REVERSE_PROXY_AUTHENTICATION_FULL_NAME",
		"REVERSE_PROXY_TRUSTED_PROXIES",
	} {
		fmt.Fprintf(&b, "%s = %s\n", key, serviceDefaults()[key])
	}
	fmt.Fprintln(&b)
	// [actions] turns loom into a local CI control plane: act_runner registers
	// against it and dials out over the mesh. DEFAULT_ACTIONS_URL = github lets
	// workflows reference `uses: actions/checkout` at setup; a mesh-local mirror
	// is a follow-up (docs/local-p2p-cicd.md).
	enabled := "false"
	if actions {
		enabled = "true"
	}
	fmt.Fprintf(&b, "[actions]\nENABLED = %s\nDEFAULT_ACTIONS_URL = github\n", enabled)
	return b.String()
}

func serviceDefaults() map[string]string {
	return map[string]string{
		"DISABLE_REGISTRATION":                   "true",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION":    "true",
		"ENABLE_REVERSE_PROXY_AUTO_REGISTRATION": "true",
		"ENABLE_REVERSE_PROXY_EMAIL":             "true",
		"ENABLE_REVERSE_PROXY_FULL_NAME":         "true",
		"REVERSE_PROXY_AUTHENTICATION_USER":      "X-WEBAUTH-USER",
		"REVERSE_PROXY_AUTHENTICATION_EMAIL":     "X-WEBAUTH-EMAIL",
		"REVERSE_PROXY_AUTHENTICATION_FULL_NAME": "X-WEBAUTH-FULLNAME",
		"REVERSE_PROXY_TRUSTED_PROXIES":          "127.0.0.0/8,::1/128",
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewLoomCmd builds the `loom` command tree (bashy front-door + any cobra host).
func NewLoomCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "loom",
		Short: "Run Gitea (the mesh git forge) as a managed external binary",
		Long: `loom runs Gitea — downloaded, sha256-verified, and cached by binmgr (not
compiled in). Expose it over the mesh with: outpost mesh service add git <addr>.`,
	}
	var o Options
	var actions bool
	var expose bool
	var service string
	var proxyTarget string
	var proxyPublicPrefix string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Download (if needed) + run gitea web on a loopback port",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("actions") {
				o.Actions = &actions
			}
			return Serve(cmd.Context(), o)
		},
	}
	serve.Flags().StringVar(&o.Version, "gitea-version", "", "Gitea release tag (default: latest)")
	serve.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "HTTP bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "HTTP port")
	serve.Flags().StringVar(&o.RootURL, "root-url", "", "Gitea ROOT_URL for UI links and clone URLs; use the stable cloudbox HTTPS URL for internet access")
	serve.Flags().BoolVar(&actions, "actions", true, "enable Gitea Actions (local CI; $LOOM_ACTIONS=0 to disable)")

	start := &cobra.Command{
		Use:   "start",
		Short: "Start loom as a detached local service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("actions") {
				o.Actions = &actions
			}
			st, err := StartDaemon(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loom started: %s pid=%d proxy=%s proxy_pid=%d log=%s\n", st.URL, st.PID, st.ProxyURL, st.ProxyPID, st.LogPath)
			if expose {
				if err := ExposeService(cmd.Context(), service, st.ProxyAddr); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "loom exposed over outpost mesh: service=%s addr=%s\n", serviceName(service), st.ProxyAddr)
				fmt.Fprintf(cmd.OutOrStdout(), "remote peers can run: outpost mesh dial --local %s %s\n", st.ProxyAddr, serviceName(service))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "point outpost app at: outpost apps add loom --url %s --require-login --trust-cloud-identity\n", st.ProxyURL)
				fmt.Fprintf(cmd.OutOrStdout(), "share over outpost mesh: outpost mesh service add git %s\n", st.ProxyAddr)
			}
			return nil
		},
	}
	start.Flags().StringVar(&o.Version, "gitea-version", "", "Gitea release tag (default: latest)")
	start.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")
	start.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "HTTP bind address")
	start.Flags().IntVar(&o.Port, "port", DefaultPort, "HTTP port")
	start.Flags().IntVar(&o.ProxyPort, "proxy-port", DefaultProxyPort, "HTTP proxy port for outpost/custom-app access")
	start.Flags().StringVar(&o.RootURL, "root-url", "", "Gitea ROOT_URL for UI links and clone URLs; use the stable cloudbox HTTPS URL for internet access")
	start.Flags().BoolVar(&actions, "actions", true, "enable Gitea Actions (local CI; $LOOM_ACTIONS=0 to disable)")
	start.Flags().BoolVar(&expose, "expose", false, "also publish loom through outpost mesh")
	start.Flags().StringVar(&service, "service", "git", "outpost mesh service name for --expose")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show loom service status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.defaults()
			st, err := readState(o.DataDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "loom stopped")
					return nil
				}
				return err
			}
			state := "stopped"
			if healthy(cmd.Context(), st.URL, 2*time.Second) {
				state = "running"
			}
			rootURL := st.RootURL
			if rootURL == "" {
				rootURL = st.URL + "/"
			}
			proxyState := "stopped"
			if st.ProxyURL != "" && healthy(cmd.Context(), st.ProxyURL, 2*time.Second) {
				proxyState = "running"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loom %s: %s root=%s pid=%d proxy=%s proxy_state=%s proxy_pid=%d data=%s log=%s\n", state, st.URL, rootURL, st.PID, st.ProxyURL, proxyState, st.ProxyPID, st.DataDir, st.LogPath)
			if state == "running" {
				if st.ProxyURL != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "point outpost app at: outpost apps add loom --url %s --require-login --trust-cloud-identity\n", st.ProxyURL)
				}
				shareAddr := st.ProxyAddr
				if shareAddr == "" {
					shareAddr = st.Addr
				}
				fmt.Fprintf(cmd.OutOrStdout(), "share over outpost mesh: outpost mesh service add git %s\n", shareAddr)
				fmt.Fprintf(cmd.OutOrStdout(), "remote peers can run: outpost mesh dial --local %s git\n", shareAddr)
			}
			return nil
		},
	}
	status.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the detached loom service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.defaults()
			st, err := StopDaemon(o.DataDir, 10*time.Second)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "loom already stopped")
					return nil
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loom stopped: %s pid=%d\n", st.URL, st.PID)
			return nil
		},
	}
	stopCmd.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")

	logs := &cobra.Command{
		Use:   "logs",
		Short: "Print the loom service log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.defaults()
			data, err := os.ReadFile(logPath(o.DataDir))
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	logs.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")

	exposeCmd := &cobra.Command{
		Use:   "expose",
		Short: "Publish a running loom service through outpost mesh",
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.defaults()
			st, err := readState(o.DataDir)
			if err != nil {
				return err
			}
			if !healthy(cmd.Context(), st.URL, 2*time.Second) {
				return fmt.Errorf("loom: service is not running at %s", st.URL)
			}
			addr := st.ProxyAddr
			if addr == "" {
				addr = st.Addr
			}
			if err := ExposeService(cmd.Context(), service, addr); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loom exposed over outpost mesh: service=%s addr=%s\n", serviceName(service), addr)
			fmt.Fprintf(cmd.OutOrStdout(), "remote peers can run: outpost mesh dial --local %s %s\n", addr, serviceName(service))
			return nil
		},
	}
	exposeCmd.Flags().StringVar(&o.DataDir, "data", "", "work dir (default ~/.agents/bashy/loom)")
	exposeCmd.Flags().StringVar(&service, "service", "git", "outpost mesh service name")

	var proxyAutoProvision bool
	proxyCmd := &cobra.Command{
		Use:    "proxy",
		Short:  "Run the Loom reverse proxy",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runProxy(ctx, o.Addr, o.ProxyPort, proxyTarget, proxyPublicPrefix, proxyAutoProvision || autoProvisionEnv())
		},
	}
	proxyCmd.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "HTTP bind address")
	proxyCmd.Flags().IntVar(&o.ProxyPort, "port", DefaultProxyPort, "HTTP proxy port")
	proxyCmd.Flags().StringVar(&proxyTarget, "target", "", "backend Gitea URL")
	proxyCmd.Flags().StringVar(&proxyPublicPrefix, "public-prefix", "", "public URL path prefix to strip before forwarding to Gitea")
	proxyCmd.Flags().BoolVar(&proxyAutoProvision, "auto-provision", false, "auto-register a Gitea account for any vouched user on first contact (default off; $LOOM_AUTO_PROVISION=1)")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached gitea binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := binmgr.ResolveGitHub(cmd.Context(), Spec(o.Version))
			if err != nil {
				return err
			}
			p, err := binmgr.Ensure(cmd.Context(), tool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t(gitea %s)\n", p, tool.Version)
			return nil
		},
	}
	pathCmd.Flags().StringVar(&o.Version, "gitea-version", "", "Gitea release tag (default: latest)")

	root.AddCommand(serve, start, status, stopCmd, logs, exposeCmd, pathCmd, proxyCmd)
	return root
}

func serviceName(service string) string {
	if strings.TrimSpace(service) == "" {
		return "git"
	}
	return strings.TrimSpace(service)
}
