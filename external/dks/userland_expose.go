// userland_expose.go — the `bashy dks expose`, `unexpose`, `exposed` client
// verbs and the hidden in-cluster `dks relay` (Sprint 487 expose contract).
//
// What an expose is: ONE local service on this Outpost host, reachable from
// the workspace's pods and from nowhere else. There is no new transport —
// the bytes ride the existing matrix-tunnel app route
// <satellite>/matrix/h/<host>/app/<app>/, so an expose is exactly an Outpost
// app plus a token guard plus an in-cluster relay that holds the token.
//
// Three objects per expose, all label-owned (dks.dhnt.io/expose=<name>): a
// Secret holding EXPOSE_TOKEN, a 1-replica Deployment running `dks relay`,
// and a Service named <name> so pods reach http://<name>:8080 (http) or
// <name>:<port> (tcp). Default-deny: nothing exposed, nothing reachable.
//
// The token discipline is the whole security story: 32 random bytes hex,
// minted here, written ONLY into the Secret, and handed to Outpost as a
// sha256 — never as argv, never to a log, never to an error. Everything that
// could print it goes through the hash instead.
package dks

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/meshagent"
	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
)

// The expose contract's label and annotations. The LABEL selects every
// object of one expose (apply, list, delete); the annotations carry what the
// `exposed` table and the run-env injection need, so neither has to keep a
// second record anywhere.
const (
	exposeLabel         = "dks.dhnt.io/expose"
	exposeExpiresAnnot  = "dks.dhnt.io/expires"
	exposeKindAnnot     = "dks.dhnt.io/expose-kind"
	exposeTargetAnnot   = "dks.dhnt.io/expose-target"
	exposeEnvAnnot      = "dks.dhnt.io/expose-env"
	exposeUpstreamAnnot = "dks.dhnt.io/expose-upstream"
)

// exposeTokenHeader is the per-request guard Outpost checks on every HTTP
// request and WebSocket upgrade to an exposed app.
const exposeTokenHeader = "X-Dhnt-Expose-Token"

// exposeTokenKey is the Secret key, and the relay's env var, holding the
// token. The relay reads it from the environment ONLY.
const exposeTokenKey = "EXPOSE_TOKEN"

// The relay's remaining environment: where to proxy and when to stop.
const (
	exposeUpstreamKey = "EXPOSE_UPSTREAM"
	exposeExpiresKey  = "EXPOSE_EXPIRES"
)

// The expose kinds this story implements. The two proxy kinds are proxy
// spellings: egress serves a CONNECT relay, git rides the http relay.
const (
	exposeKindHTTP   = "http"
	exposeKindTCP    = "tcp"
	exposeKindEgress = "proxy:egress"
	exposeKindGit    = "proxy:git"
)

// exposeHTTPPort is the port an http expose's Service publishes: pods reach
// it as http://<name>:8080, which is what `dks run` injects.
const exposeHTTPPort = 8080

// The egress record kind, Outpost target and Service port. The CLI accepts
// proxy:egress as the target spelling; the record, the relay mode and the
// exposed table all use the short kind egress.
const (
	exposeEgressKind   = "egress"
	exposeEgressTarget = "egress://"
	exposeEgressPort   = 3128
)

// The git record kind, the Outpost target prefix and the default git host.
// The CLI accepts proxy:git; the record and the exposed table use the short
// kind git, Outpost gets git+<upstream>, and the relay rides the plain http
// mode — Outpost's git app does the smart-HTTP filtering and injects the
// HOST's credential, never the pod's.
const (
	exposeGitKind         = "git"
	exposeGitTargetPrefix = "git+"
	defaultGitUpstream    = "https://github.com"
)

// egressNoProxyVar and egressNoProxy are the bypass variable and list every
// egress run inherits alongside the proxy variables.
const (
	egressNoProxyVar = "NO_PROXY"
	egressNoProxy    = ".svc,.cluster.local,localhost,127.0.0.1"
)

// exposeSecretRefPrefix names the pseudo-vault of expose-provided Secret
// entries: an expose variable the dag manifest Env line cannot carry (it
// splits values on whitespace and commas, so the comma-separated NO_PROXY
// bypass list is unrepresentable there) rides the run Secret under a
// dks-expose ref instead, and the pod still receives it via secretKeyRef.
const exposeSecretRefPrefix = "dks-expose"

// exposeSecretRef is the manifest Secrets: ref wiring one expose-provided
// Secret entry to its environment variable.
func exposeSecretRef(envName string) string {
	return exposeSecretRefPrefix + "=" + envName
}

// manifestEnvValueOK reports whether v can ride a dag manifest Env: line:
// empty, otherwise free of whitespace, commas, controls and ${ — the
// manifest's own list-token rule, mirrored here so a variable the line
// cannot carry is routed to the run Secret instead of failing generation.
func manifestEnvValueOK(v string) bool {
	if v == "" {
		return true
	}
	if strings.Contains(v, "${") {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || r == ' ' || r == '\t' || r == ',' {
			return false
		}
	}
	return true
}

// TTL bounds of one expose: the default is short on purpose, and nothing
// outlives a day without being re-exposed.
const (
	defaultExposeTTL = time.Hour
	maxExposeTTL     = 24 * time.Hour
)

// exposeNameRE is the shape of an expose name: a lowercase DNS label that
// starts with a letter, because the name IS a Service name (RFC 1123 label,
// and the API additionally refuses a leading digit on a Service).
var exposeNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// exposeBuiltinApps are the Outpost built-in and admin-grade app names. An
// expose NAME that collides with one of them would shadow the built-in on
// the host's app route; a TARGET that names one would publish the host's own
// administration. Both are refused without --force.
var exposeBuiltinApps = []string{
	"admin", "agent", "console", "desktop", "files", "periscope",
	"shell", "ssh", "sshd", "terminal",
}

// exposeAdminPorts are the local ports of this host's own remote-control
// surfaces: ssh, RDP and VNC (the "desktop" app's transport). Outpost's
// admin UI is served through the satellite edge rather than a local port, so
// the NAME rule above — not a port — is what guards it.
var exposeAdminPorts = []int{22, 2222, 3389, 5900, 5901}

// exposeAppSpec is one app registration on the Outpost host side. Only the
// HASH of the token is in it: the token itself never leaves this process
// except into the cluster Secret.
type exposeAppSpec struct {
	App         string
	Target      string
	TokenSHA256 string
	Expires     time.Time
	// Allow is the egress allow list (host or *.domain[:port] entries).
	Allow []string
}

// outpostApps is the Outpost host side behind one seam, so no test execs the
// installed outpost.
type outpostApps interface {
	Add(ctx context.Context, spec exposeAppSpec) error
	Rm(ctx context.Context, app string) error
}

// execOutpostApps is the production host side: it execs the installed
// outpost. Nothing secret rides stdin or the environment — only the sha256
// is passed, and it is not a secret.
type execOutpostApps struct{}

// addArgv is the argv of one registration. It is a method of its own so the
// "no token in argv" rule is checkable without execing anything.
func (execOutpostApps) addArgv(spec exposeAppSpec) []string {
	argv := []string{
		"apps", "add", spec.App,
		"--target", spec.Target,
		"--expose-token-sha256", spec.TokenSHA256,
		"--expose-expires", spec.Expires.UTC().Format(time.RFC3339),
	}
	if len(spec.Allow) > 0 {
		argv = append(argv, "--allow", strings.Join(spec.Allow, ","))
	}
	return argv
}

func (e execOutpostApps) Add(ctx context.Context, spec exposeAppSpec) error {
	return exposeOutpostError(ExecOutpost(ctx, e.addArgv(spec)...))
}

func (execOutpostApps) Rm(ctx context.Context, app string) error {
	return exposeOutpostError(ExecOutpost(ctx, "apps", "rm", app))
}

// exposeOutpostError names the one failure an operator can act on: an
// expose is registered on the Outpost host, so without outpost there is
// nothing to expose through.
func exposeOutpostError(err error) error {
	if errors.Is(err, meshagent.ErrNotFound) {
		return errors.New("the outpost agent isn't available on this machine: an expose is an Outpost app, so there is nothing to expose through — install the outpost mesh agent (or set $OUTPOST_BIN)")
	}
	return err
}

// exposeRecord is one expose as the plane holds it.
type exposeRecord struct {
	Name    string
	Kind    string
	Target  string
	Expires time.Time
	Port    int      // the Service port; 8080 for http
	Env     []string // the recorded --env entries, {url} unsubstituted
	// Upstream is a git expose's git host (default https://github.com): the
	// value the run env's insteadOf rewrite matches against.
	Upstream string
}

// live reports whether the expose is still inside its TTL.
func (r exposeRecord) live(now time.Time) bool {
	return r.Expires.IsZero() || now.Before(r.Expires)
}

// address is what a pod uses to reach the expose: the http URL, the egress
// proxy URL, or the tcp host:port. It is also the {url} substitution of the
// recorded env.
func (r exposeRecord) address() string {
	if r.Kind == exposeKindTCP {
		return fmt.Sprintf("%s:%d", r.Name, r.Port)
	}
	port := r.Port
	if port == 0 {
		port = exposeHTTPPort
	}
	return fmt.Sprintf("http://%s:%d", r.Name, port)
}

// envVarName is the DKS_<NAME>_ suffix variable of one expose: URL for an
// http expose, ADDR for a tcp one.
func (r exposeRecord) envVarName() string {
	suffix := "_URL"
	if r.Kind == exposeKindTCP {
		suffix = "_ADDR"
	}
	return "DKS_" + strings.ToUpper(strings.ReplaceAll(r.Name, "-", "_")) + suffix
}

// exposeEnv extends the run seams with the Outpost host side, the token
// minter and the satellite/host identity the app route is built from.
type exposeEnv struct {
	*runEnv
	apps outpostApps
	// mintToken mints one 32-byte hex expose token.
	mintToken func() (string, error)
	// tunnelBase resolves the satellite base of the selected plane — the
	// same derivation attach uses.
	tunnelBase func(ctxRec dagrun.Context) (string, error)
	// outpostHost is this machine's Outpost host identity: the <host>
	// segment of the matrix app route.
	outpostHost func() (string, error)
	// relayDialCtx is the relay's base context; tests pin it.
	relayDialCtx func() context.Context
}

func defaultExposeEnv(env *userlandEnv) *exposeEnv {
	return &exposeEnv{
		runEnv:       defaultRunEnv(env),
		apps:         execOutpostApps{},
		mintToken:    mintExposeToken,
		tunnelBase:   defaultTunnelBase,
		outpostHost:  defaultOutpostHost,
		relayDialCtx: context.Background,
	}
}

// newUserlandExposeCmds builds expose, unexpose, exposed and the hidden
// relay over the production seams.
func newUserlandExposeCmds(env *userlandEnv) []*cobra.Command {
	return newExposeCmds(defaultExposeEnv(env))
}

func newExposeCmds(env *exposeEnv) []*cobra.Command {
	return []*cobra.Command{
		newExposeCmd(env),
		newUnexposeCmd(env),
		newExposedCmd(env),
		newRelayCmd(env),
	}
}

// outpostHostEnv overrides this machine's Outpost host identity.
const outpostHostEnv = "BASHY_DKS_OUTPOST_HOST"

// defaultOutpostHost is $BASHY_DKS_OUTPOST_HOST when set, else this
// machine's host name — the name Outpost registers a host under, and the
// <host> segment of its app route. The override exists for a host whose
// Outpost identity differs from its local name.
func defaultOutpostHost() (string, error) {
	if h := strings.TrimSpace(os.Getenv(outpostHostEnv)); h != "" {
		if !validLabelValue(h) {
			return "", fmt.Errorf("$%s = %q is not a valid Outpost host name — nothing ran", outpostHostEnv, h)
		}
		return h, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("this machine's Outpost host identity: %v; set $%s — nothing ran", err, outpostHostEnv)
	}
	// A host name may be fully qualified; the app route takes the label.
	h, _, _ = strings.Cut(strings.TrimSpace(h), ".")
	if !validLabelValue(h) {
		return "", fmt.Errorf("this machine's host name %q is not a valid Outpost host name; set $%s — nothing ran", h, outpostHostEnv)
	}
	return h, nil
}

// mintExposeToken mints one expose token: 32 bytes from crypto/rand, hex.
func mintExposeToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint expose token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// exposeOptions are the flags of one `dks expose`.
type exposeOptions struct {
	as       string
	ttl      time.Duration
	env      []string
	allow    []string
	upstream string
	force    bool
}

func newExposeCmd(env *exposeEnv) *cobra.Command {
	var o exposeOptions
	cmd := &cobra.Command{
		Use:   "expose <http://127.0.0.1:PORT | tcp://host:port | proxy:egress | proxy:git> --as <name> [--ttl 1h] [--allow a,b] [--env K=V]... [--force]",
		Short: "Make one local service reachable from the selected workspace's pods",
		Long: `expose makes exactly ONE service on this machine reachable from the pods
of the selected workspace, for a bounded time:

  bashy dks expose http://127.0.0.1:9000 --as web
  bashy dks expose tcp://127.0.0.1:5432 --as db --ttl 4h
  bashy dks expose http://127.0.0.1:9000 --as llm --env ANTHROPIC_BASE_URL={url}
  bashy dks expose proxy:git --as git --allow owner/repo,owner/*

Pods then reach it at http://<name>:8080 (http) or <name>:<port> (tcp), and
'bashy dks run' injects DKS_<NAME>_URL / DKS_<NAME>_ADDR for every live
expose. A proxy:git expose additionally injects GIT_CONFIG_* insteadOf
entries, so a plain git clone in a pod goes through the relay with no
credential in the pod. Default-deny holds: nothing exposed is nothing
reachable.

The expose is registered on this machine's Outpost host with a one-off token
and an expiry; past the expiry it stops answering at once, with no restart.
Refused without --force: an admin-grade target (ssh, desktop) and a name
that collides with an Outpost built-in app.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.expose(cmd, o, args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.as, "as", "", "the name pods reach the service by (a lowercase DNS label)")
	f.DurationVar(&o.ttl, "ttl", defaultExposeTTL, "how long the expose lives (max 24h)")
	f.StringArrayVar(&o.env, "env", nil, "record one variable for every run in the workspace: K=V, with {url} substituted; repeatable")
	f.StringSliceVar(&o.allow, "allow", nil, "allowlist for the proxy kinds (proxy:egress: host,*.domain[:port]; proxy:git: owner/repo,owner/*)")
	f.StringVar(&o.upstream, "upstream", "", "with proxy:git: the git host to proxy (default "+defaultGitUpstream+")")
	f.BoolVar(&o.force, "force", false, "expose an admin-grade target or a built-in app name anyway")
	return cmd
}

// exposeTarget is a parsed target: its kind, the string Outpost registers,
// and the tcp port a tcp expose publishes.
type exposeTarget struct {
	kind   string
	target string
	port   int
}

// parseExposeTarget parses the target grammar of the contract. The two proxy
// kinds parse here and are refused as unimplemented by the caller, so a
// typo in them is still a parse error rather than a silent "not yet".
func parseExposeTarget(raw string) (exposeTarget, error) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case exposeKindEgress:
		return exposeTarget{kind: exposeKindEgress, target: raw}, nil
	case exposeKindGit:
		return exposeTarget{kind: exposeKindGit, target: raw}, nil
	}
	switch {
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return exposeTarget{}, fmt.Errorf("target %q is not a usable http URL — nothing ran", raw)
		}
		return exposeTarget{kind: exposeKindHTTP, target: raw}, nil
	case strings.HasPrefix(raw, "tcp://"):
		hostport := strings.TrimPrefix(raw, "tcp://")
		host, portText, err := net.SplitHostPort(hostport)
		if err != nil || host == "" {
			return exposeTarget{}, fmt.Errorf("target %q is not tcp://host:port — nothing ran", raw)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return exposeTarget{}, fmt.Errorf("target %q names no usable port — nothing ran", raw)
		}
		return exposeTarget{kind: exposeKindTCP, target: raw, port: port}, nil
	}
	return exposeTarget{}, fmt.Errorf("target %q: want http://127.0.0.1:PORT, tcp://host:port, %s or %s — nothing ran", raw, exposeKindEgress, exposeKindGit)
}

// cleanEgressAllow splits, trims and validates the --allow list of a
// proxy:egress expose. Entries are host or *.domain, each with an optional
// :port. An empty list, an empty entry, or a malformed entry refuses the
// whole expose before anything exists.
func cleanEgressAllow(raw []string) ([]string, error) {
	var out []string
	for _, s := range raw {
		for _, part := range strings.Split(s, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				if strings.TrimSpace(s) == "" && len(raw) == 1 && len(out) == 0 {
					return nil, fmt.Errorf("--allow is required for a %s expose: pass --allow host,*.domain[:port] — nothing ran", exposeKindEgress)
				}
				return nil, fmt.Errorf("--allow %q: want host or *.domain, each with an optional :port — nothing ran", s)
			}
			if err := validateEgressAllowEntry(p); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--allow is required for a %s expose: pass --allow host,*.domain[:port] — nothing ran", exposeKindEgress)
	}
	return out, nil
}

// validateEgressAllowEntry checks one allow entry: a hostname or *.domain
// with an optional numeric :port.
func validateEgressAllowEntry(s string) error {
	bad := func() error {
		return fmt.Errorf("--allow %q: want host or *.domain, each with an optional :port — nothing ran", s)
	}
	if s == "" || strings.ContainsAny(s, " \t/?#@") {
		return bad()
	}
	host := s
	if strings.Contains(s, ":") {
		h, portText, err := net.SplitHostPort(s)
		if err != nil || h == "" {
			return bad()
		}
		port, perr := strconv.Atoi(portText)
		if perr != nil || port < 1 || port > 65535 {
			return bad()
		}
		host = h
	}
	if strings.HasPrefix(host, "*.") {
		rest := strings.TrimPrefix(host, "*.")
		if rest == "" || strings.Contains(rest, "*") || !strings.Contains(rest, ".") {
			return bad()
		}
		if !validEgressHostname(rest) {
			return bad()
		}
		return nil
	}
	if strings.Contains(host, "*") {
		return bad()
	}
	if !validEgressHostname(host) {
		return bad()
	}
	return nil
}

// validEgressHostname is a permissive dot-separated hostname check: labels of
// letters, digits and hyphens, never starting or ending with a hyphen.
func validEgressHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// cleanGitAllow splits, trims and validates the --allow list of a proxy:git
// expose. Entries are owner/repo or owner/*; Outpost proxies only the git
// smart-HTTP traffic of the listed repositories, so a bad entry refusing the
// whole expose keeps the allowlist honest.
func cleanGitAllow(raw []string) ([]string, error) {
	var out []string
	for _, s := range raw {
		for _, part := range strings.Split(s, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				if strings.TrimSpace(s) == "" && len(raw) == 1 && len(out) == 0 {
					return nil, fmt.Errorf("--allow is required for a %s expose: pass --allow owner/repo,owner/* — nothing ran", exposeKindGit)
				}
				return nil, fmt.Errorf("--allow %q: want owner/repo or owner/* — nothing ran", s)
			}
			if err := validateGitAllowEntry(p); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--allow is required for a %s expose: pass --allow owner/repo,owner/* — nothing ran", exposeKindGit)
	}
	return out, nil
}

// gitAllowNameRE is one owner or repo name of a git allow entry: a
// git-host name of letters, digits, dot, hyphen and underscore, starting
// with a letter or digit.
var gitAllowNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateGitAllowEntry checks one git allow entry: owner/repo or owner/*.
func validateGitAllowEntry(s string) error {
	bad := func() error {
		return fmt.Errorf("--allow %q: want owner/repo or owner/* — nothing ran", s)
	}
	owner, repo, hasSlash := strings.Cut(s, "/")
	if !hasSlash || !gitAllowNameRE.MatchString(owner) {
		return bad()
	}
	if repo == "*" {
		return nil
	}
	if !gitAllowNameRE.MatchString(repo) {
		return bad()
	}
	return nil
}

// parseGitUpstream validates --upstream of a proxy:git expose: an http(s)
// git host URL, defaulting to the public git host. The trailing slash is
// trimmed so the run env's insteadOf value can append its own.
func parseGitUpstream(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		u = defaultGitUpstream
	}
	bad := func() error {
		return fmt.Errorf("--upstream %q: want an http(s) git host URL such as %s — nothing ran", raw, defaultGitUpstream)
	}
	if strings.ContainsAny(u, " \t") || strings.ContainsAny(u, "\r\n\x00") {
		return "", bad()
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", bad()
	}
	return strings.TrimSuffix(u, "/"), nil
}

// adminGrade names why a target or name is admin-grade, "" when it is not.
// This is the --force gate: it refuses publishing the host's own
// administration by accident, and refuses shadowing a built-in app name.
func adminGrade(name string, tgt exposeTarget) string {
	for _, builtin := range exposeBuiltinApps {
		if name == builtin {
			return fmt.Sprintf("app name %q collides with the Outpost built-in app of the same name", name)
		}
	}
	port := tgt.port
	if tgt.kind == exposeKindHTTP {
		if u, err := url.Parse(tgt.target); err == nil {
			if p, perr := strconv.Atoi(u.Port()); perr == nil {
				port = p
			}
			first, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
			for _, builtin := range exposeBuiltinApps {
				if first == builtin {
					return fmt.Sprintf("target path names the Outpost built-in app %q", builtin)
				}
			}
		}
	}
	for _, admin := range exposeAdminPorts {
		if port == admin {
			return fmt.Sprintf("port %d is one of this host's own remote-control surfaces (ssh, desktop)", port)
		}
	}
	return ""
}

// expose is `dks expose`. Order is fail-closed: parse, validate and refuse
// FIRST (no host call, no plane call), then register the app with Outpost,
// then apply the three objects. Nothing exists until all of it succeeded.
func (env *exposeEnv) expose(cmd *cobra.Command, o exposeOptions, args []string) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks expose: %s\n", err)
		return errUserlandHandled
	}
	if len(args) != 1 {
		fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks expose <http://127.0.0.1:PORT | tcp://host:port | proxy:egress | proxy:git> --as <name>")
		return errUserlandHandled
	}
	name := strings.TrimSpace(o.as)
	if name == "" {
		return fail(errors.New("--as <name> is required: it is the name pods reach the service by — nothing ran"))
	}
	if !exposeNameRE.MatchString(name) {
		return fail(fmt.Errorf("--as %q: want a lowercase DNS label starting with a letter (a-z, 0-9, -), at most 63 characters — nothing ran", name))
	}
	tgt, err := parseExposeTarget(args[0])
	if err != nil {
		return fail(err)
	}
	if o.ttl <= 0 || o.ttl > maxExposeTTL {
		return fail(fmt.Errorf("--ttl %s: want a positive duration of at most %s — nothing ran", o.ttl, maxExposeTTL))
	}
	recorded, err := parseExposeEnv(o.env)
	if err != nil {
		return fail(err)
	}
	var egressAllow []string
	var gitAllow []string
	gitUpstream := ""
	if tgt.kind == exposeKindEgress {
		cleaned, err := cleanEgressAllow(o.allow)
		if err != nil {
			return fail(err)
		}
		egressAllow = cleaned
	} else if tgt.kind == exposeKindGit {
		cleaned, err := cleanGitAllow(o.allow)
		if err != nil {
			return fail(err)
		}
		gitAllow = cleaned
		upstream, err := parseGitUpstream(o.upstream)
		if err != nil {
			return fail(err)
		}
		gitUpstream = upstream
	} else {
		if len(o.allow) > 0 {
			return fail(fmt.Errorf("--allow applies to %s and %s only, not to an %s target — nothing ran", exposeKindEgress, exposeKindGit, tgt.kind))
		}
		if strings.TrimSpace(o.upstream) != "" {
			return fail(fmt.Errorf("--upstream applies to %s only, not to an %s target — nothing ran", exposeKindGit, tgt.kind))
		}
	}
	if why := adminGrade(name, tgt); why != "" && !o.force {
		// One audit line per refusal, with the reason named.
		return fail(fmt.Errorf("%s — refused; pass --force to expose it anyway; nothing ran", why))
	}
	ctxRec, err := env.selected()
	if err != nil {
		return fail(err)
	}
	image, _, err := env.resolveImage("")
	if err != nil {
		return fail(err)
	}
	base, err := env.tunnelBase(ctxRec)
	if err != nil {
		return fail(err)
	}
	host, err := env.outpostHost()
	if err != nil {
		return fail(err)
	}
	upstream := tunnelAppURL(base, host, name)

	token, err := env.mintToken()
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256([]byte(token))
	expires := env.now().Add(o.ttl).UTC().Truncate(time.Second)
	spec := exposeAppSpec{App: name, Target: tgt.target, TokenSHA256: hex.EncodeToString(sum[:]), Expires: expires}
	rec := exposeRecord{Name: name, Kind: tgt.kind, Target: tgt.target, Expires: expires, Port: tgt.port, Env: recorded}
	if tgt.kind == exposeKindEgress {
		spec.Target = exposeEgressTarget
		spec.Allow = egressAllow
		rec.Kind = exposeEgressKind
		rec.Target = strings.Join(egressAllow, ",")
		rec.Port = exposeEgressPort
	}
	if tgt.kind == exposeKindGit {
		spec.Target = exposeGitTargetPrefix + gitUpstream
		spec.Allow = gitAllow
		rec.Kind = exposeGitKind
		rec.Target = strings.Join(gitAllow, ",")
		rec.Port = exposeHTTPPort
		rec.Upstream = gitUpstream
	}
	if rec.Kind == exposeKindHTTP {
		rec.Port = exposeHTTPPort
	}

	client, err := env.planeClient(ctxRec)
	if err != nil {
		return fail(err)
	}
	if err := env.apps.Add(cmd.Context(), spec); err != nil {
		return fail(fmt.Errorf("register app %s with Outpost: %v — nothing ran", name, err))
	}
	objs, err := exposeObjects(rec, ctxRec.WorkspaceID, image, upstream, token)
	if err != nil {
		return fail(err)
	}
	for _, obj := range objs {
		if err := applyObject(cmd.Context(), client, obj); err != nil {
			// A half-applied expose is removed: nothing reachable, nothing left.
			_ = env.removeExpose(cmd.Context(), client, name)
			_ = env.apps.Rm(cmd.Context(), name)
			return fail(planeError(ctxRec, err))
		}
	}
	// The audit line: one per expose, token-free by construction.
	fmt.Fprintf(cmd.ErrOrStderr(), "dks: expose %s %s %s expires %s\n", name, rec.Kind, rec.Target, expires.Format(time.RFC3339))
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n", rec.address())
	return nil
}

// tunnelAppURL is the matrix-tunnel address of an Outpost app:
// <satellite>/matrix/h/<host>/app/<app>/. The ONLY transport an expose uses.
func tunnelAppURL(base, host, app string) string {
	return strings.TrimSuffix(base, "/") + "/matrix/h/" + host + "/app/" + app + "/"
}

// parseExposeEnv validates the recorded --env entries. Each must be K=V with
// a valid variable name; {url} in the value is substituted per run.
func parseExposeEnv(specs []string) ([]string, error) {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		k, v, hasValue := strings.Cut(s, "=")
		if !hasValue || !validEnvName(k) {
			return nil, fmt.Errorf("--env %q: want K=V with a valid variable name — nothing ran", s)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// exposeObjects renders the three label-owned objects of one expose, in
// apply order: the Secret first (the Deployment references it), then the
// Deployment, then the Service.
func exposeObjects(rec exposeRecord, workspaceID, image, upstream, token string) ([]map[string]any, error) {
	if !validLabelValue(workspaceID) {
		return nil, fmt.Errorf("workspace id %q is not a label-safe id — nothing ran", workspaceID)
	}
	envJSON := "[]"
	if len(rec.Env) > 0 {
		b, err := json.Marshal(rec.Env)
		if err != nil {
			return nil, fmt.Errorf("encode recorded env: %w", err)
		}
		envJSON = string(b)
	}
	labels := map[string]any{
		dagrun.LabelManagedBy: dagrun.ManagedByBashy,
		dagrun.LabelWorkspace: workspaceID,
		exposeLabel:           rec.Name,
	}
	annots := map[string]any{
		exposeExpiresAnnot: rec.Expires.UTC().Format(time.RFC3339),
		exposeKindAnnot:    rec.Kind,
		exposeTargetAnnot:  rec.Target,
		exposeEnvAnnot:     envJSON,
	}
	if rec.Upstream != "" {
		annots[exposeUpstreamAnnot] = rec.Upstream
	}
	meta := func(name string) map[string]any {
		return map[string]any{"name": name, "labels": cloneAnyMap(labels), "annotations": cloneAnyMap(annots)}
	}
	objName := "dks-expose-" + rec.Name
	selector := map[string]any{
		dagrun.LabelManagedBy: dagrun.ManagedByBashy,
		exposeLabel:           rec.Name,
	}
	// A git expose rides the plain http relay: the existing http relay is
	// enough, because Outpost's git app does the smart-HTTP filtering and
	// injects the HOST's credential beyond it.
	relayMode := rec.Kind
	if relayMode == exposeGitKind {
		relayMode = exposeKindHTTP
	}
	// limits == requests, exactly as a run's container does: a relay that
	// cannot be placed is better than one a namespace default resizes.
	resources := map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
		"limits":   map[string]any{"cpu": "100m", "memory": "64Mi"},
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta(objName),
		"stringData": map[string]any{exposeTokenKey: token},
	}
	container := map[string]any{
		"name":  "relay",
		"image": image,
		"command": []any{
			"bashy", "dks", "relay",
			"--mode", relayMode,
			"--listen", fmt.Sprintf(":%d", rec.Port),
		},
		"env": []any{
			map[string]any{"name": exposeUpstreamKey, "value": upstream},
			map[string]any{"name": exposeExpiresKey, "value": rec.Expires.UTC().Format(time.RFC3339)},
			map[string]any{"name": exposeTokenKey, "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": objName, "key": exposeTokenKey},
			}},
		},
		"ports":     []any{map[string]any{"containerPort": rec.Port}},
		"resources": resources,
	}
	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   meta(objName),
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": cloneAnyMap(selector)},
			"template": map[string]any{
				"metadata": map[string]any{"labels": cloneAnyMap(labels)},
				"spec": map[string]any{
					"containers": []any{container},
				},
			},
		},
	}
	service := map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   meta(rec.Name),
		"spec": map[string]any{
			"selector": cloneAnyMap(selector),
			"ports": []any{map[string]any{
				"name":       rec.Kind,
				"port":       rec.Port,
				"targetPort": rec.Port,
				"protocol":   "TCP",
			}},
		},
	}
	return []map[string]any{secret, deployment, service}, nil
}

func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// applyObject server-side applies one object, exactly the way applySecret
// does: label-owned, field-managed by bashy, one object per call so a
// failure names the object that failed.
func applyObject(ctx context.Context, client *cluster.Client, obj map[string]any) error {
	payload, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("cluster: encode %v: %w", obj["kind"], err)
	}
	payload = append(payload, '\n')
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	argv = append(argv, "apply", "--server-side", "--field-manager="+dagrun.ManagedByBashy, "-f", "-")
	_, stderr, exit, err := client.Runner.Run(ctx, argv, payload)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return fmt.Errorf("%w: applying the %v exited %d: %s", cluster.ErrPlaneRejected, obj["kind"], exit, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// exposeStopKinds is exactly what exposeObjects can create — never a run's
// kinds, never everything.
const exposeStopKinds = "secrets,deployments,services"

// removeExpose deletes the objects of one expose by its label, and nothing
// else: the selector carries both the bashy ownership label and the expose
// name, so no other expose's and no run's objects are ever in scope.
func (env *exposeEnv) removeExpose(ctx context.Context, client *cluster.Client, name string) error {
	if !validLabelValue(name) {
		return fmt.Errorf("%w: %s=%q", cluster.ErrInvalidSelector, exposeLabel, name)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + exposeLabel + "=" + name
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	argv = append(argv, "delete", exposeStopKinds, "-l", sel, "--wait=false")
	_, stderr, exit, err := client.Runner.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return fmt.Errorf("%w: kubectl delete exited %d: %s", cluster.ErrPlaneRejected, exit, strings.TrimSpace(string(stderr)))
	}
	return nil
}

func newUnexposeCmd(env *exposeEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "unexpose <name>",
		Short: "Remove one expose: the Outpost app and its labelled objects",
		Long: `unexpose removes exactly one expose — the app registration on this
machine's Outpost host, and the objects the expose label selects:

  bashy dks unexpose web

Nothing else of the workspace is touched: the delete is selected by
dks.dhnt.io/expose=<name>, never by name or namespace.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := func(err error) error {
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks unexpose: %s\n", err)
				return errUserlandHandled
			}
			name := strings.TrimSpace(args[0])
			if !exposeNameRE.MatchString(name) {
				return fail(fmt.Errorf("%q is not an expose name ('bashy dks exposed' lists them) — nothing ran", name))
			}
			ctxRec, err := env.selected()
			if err != nil {
				return fail(err)
			}
			client, err := env.planeClient(ctxRec)
			if err != nil {
				return fail(err)
			}
			// Outpost first: the guard goes away before the relay does, so
			// there is no window where the relay holds a live token.
			if err := env.apps.Rm(cmd.Context(), name); err != nil {
				return fail(fmt.Errorf("remove app %s from Outpost: %v", name, err))
			}
			if err := env.removeExpose(cmd.Context(), client, name); err != nil {
				return fail(planeError(ctxRec, err))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "dks: unexpose %s\n", name)
			fmt.Fprintf(cmd.OutOrStdout(), "dks: removed expose %s\n", name)
			return nil
		},
	}
}

func newExposedCmd(env *exposeEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "exposed",
		Short: "List the selected workspace's exposes",
		Long: `exposed lists the exposes of the selected workspace:

  NAME  KIND  TARGET  EXPIRES  STATE

STATE is live while the expose is inside its TTL and expired after it. An
expired expose answers nothing; remove it with 'bashy dks unexpose'.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error) error {
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks exposed: %s\n", err)
				return errUserlandHandled
			}
			ctxRec, err := env.selected()
			if err != nil {
				return fail(err)
			}
			recs, err := env.exposes(cmd.Context(), ctxRec)
			if err != nil {
				return fail(planeError(ctxRec, err))
			}
			writeExposes(cmd.OutOrStdout(), recs, env.now())
			return nil
		},
	}
}

// writeExposes prints the exposes in name order.
func writeExposes(w io.Writer, recs []exposeRecord, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tTARGET\tEXPIRES\tSTATE")
	for _, r := range recs {
		expires := "-"
		if !r.Expires.IsZero() {
			expires = r.Expires.UTC().Format(time.RFC3339)
		}
		state := "expired"
		if r.live(now) {
			state = "live"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, orDash(r.Kind), orDash(r.Target), expires, state)
	}
	_ = tw.Flush()
}

// exposes lists the workspace's exposes through the run seam, so a test can
// script records without a plane.
func (env *runEnv) exposes(ctx context.Context, ctxRec dagrun.Context) ([]exposeRecord, error) {
	if env.listExposes == nil {
		return nil, nil
	}
	return env.listExposes(ctx, ctxRec)
}

// planeExposes reads the workspace's exposes from the labelled Services:
// one Service per expose, carrying every annotation the table and the run
// env need, so no second record exists anywhere.
func planeExposes(ctx context.Context, env *runEnv, ctxRec dagrun.Context) ([]exposeRecord, error) {
	if !validLabelValue(ctxRec.WorkspaceID) {
		return nil, fmt.Errorf("workspace id %q is not a label-safe id", ctxRec.WorkspaceID)
	}
	client, err := env.planeClient(ctxRec)
	if err != nil {
		return nil, err
	}
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," +
		dagrun.LabelWorkspace + "=" + ctxRec.WorkspaceID + "," + exposeLabel
	argv = append(argv, "get", "services", "-l", sel, "-o", "json")
	stdout, stderr, exit, err := client.Runner.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return nil, fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return nil, fmt.Errorf("%w: kubectl get services exited %d: %s", cluster.ErrPlaneRejected, exit, strings.TrimSpace(string(stderr)))
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Ports []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(stdout, &list); err != nil {
		return nil, fmt.Errorf("parse kubectl get services output: %w", err)
	}
	var out []exposeRecord
	for _, it := range list.Items {
		name := strings.TrimSpace(it.Metadata.Labels[exposeLabel])
		if name == "" {
			continue
		}
		rec := exposeRecord{
			Name:     name,
			Kind:     it.Metadata.Annotations[exposeKindAnnot],
			Target:   it.Metadata.Annotations[exposeTargetAnnot],
			Upstream: it.Metadata.Annotations[exposeUpstreamAnnot],
		}
		if raw := strings.TrimSpace(it.Metadata.Annotations[exposeExpiresAnnot]); raw != "" {
			if t, perr := time.Parse(time.RFC3339, raw); perr == nil {
				rec.Expires = t
			}
		}
		if len(it.Spec.Ports) > 0 {
			rec.Port = it.Spec.Ports[0].Port
		}
		if raw := strings.TrimSpace(it.Metadata.Annotations[exposeEnvAnnot]); raw != "" && raw != "[]" {
			var recorded []string
			if json.Unmarshal([]byte(raw), &recorded) == nil {
				rec.Env = recorded
			}
		}
		out = append(out, rec)
	}
	sortExposes(out)
	return out, nil
}

func sortExposes(recs []exposeRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j].Name < recs[j-1].Name; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

// gitConfigPrefix is the family of environment variables that rewrites git
// URLs in the pod: a plain `git clone <upstream>/owner/repo` goes through
// the relay instead, with NO credential in the pod — the HOST's credential
// is injected beyond the relay, by Outpost.
const gitConfigPrefix = "GIT_CONFIG_"

// exposeRunEnv is the environment every run in the workspace inherits from
// its LIVE exposes: the address variable, plus the recorded entries with
// {url} substituted, plus the proxy variables for a live egress expose. An
// expired expose contributes nothing — the TTL cut is what makes an expose
// safe to leave lying around.
func exposeRunEnv(recs []exposeRecord, now time.Time) map[string]string {
	out := map[string]string{}
	gitCount := 0
	for _, r := range recs {
		if !r.live(now) {
			continue
		}
		addr := r.address()
		out[r.envVarName()] = addr
		if r.Kind == exposeEgressKind {
			out["HTTPS_PROXY"] = addr
			out["HTTP_PROXY"] = addr
			out[egressNoProxyVar] = egressNoProxy
		}
		if r.Kind == exposeGitKind && r.Upstream != "" {
			i := strconv.Itoa(gitCount)
			out[gitConfigPrefix+"KEY_"+i] = "url." + addr + "/.insteadOf"
			out[gitConfigPrefix+"VALUE_"+i] = strings.TrimSuffix(r.Upstream, "/") + "/"
			gitCount++
		}
		for _, kv := range r.Env {
			k, v, hasValue := strings.Cut(kv, "=")
			if !hasValue || !validEnvName(k) {
				continue
			}
			out[k] = strings.ReplaceAll(v, "{url}", addr)
		}
	}
	if gitCount > 0 {
		out[gitConfigPrefix+"COUNT"] = strconv.Itoa(gitCount)
	}
	return out
}

// relayOptions are the flags of one `dks relay`.
type relayOptions struct {
	mode       string
	listen     string
	upstream   string
	pathPrefix string
	expires    string
}

func newRelayCmd(env *exposeEnv) *cobra.Command {
	var o relayOptions
	cmd := &cobra.Command{
		Use:   "relay --mode http|tcp|egress",
		Short: "Run the in-cluster expose relay (internal)",
		Long: `relay is the in-cluster half of an expose: it holds the expose token and
reaches the exposed service only through the Outpost app route.

  --mode http    listen and reverse-proxy to the app route
  --mode tcp     listen and bridge each connection over one WebSocket upgrade
  --mode egress  listen as an HTTP CONNECT proxy, bridging each CONNECT over
                 one ?dst= WebSocket upgrade of the app route

The token comes from $` + exposeTokenKey + ` and from nowhere else; it is never
an argument and never printed. Past its expiry the relay exits non-zero.
'bashy dks expose' runs this for you; it is not meant to be typed.`,
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.relay(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.mode, "mode", "", "http, tcp or egress")
	f.StringVar(&o.listen, "listen", ":8080", "address to listen on")
	f.StringVar(&o.upstream, "upstream", "", "the app route to reach (default $"+exposeUpstreamKey+")")
	f.StringVar(&o.pathPrefix, "path-prefix", "", "with --mode http: prefix every proxied path")
	f.StringVar(&o.expires, "expires", "", "when the expose dies (default $"+exposeExpiresKey+")")
	return cmd
}

// relay is `dks relay`. The expiry is checked BEFORE anything binds: past
// it, the relay exits non-zero having served nothing.
func (env *exposeEnv) relay(cmd *cobra.Command, o relayOptions) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks relay: %s\n", err)
		return errUserlandHandled
	}
	token := strings.TrimSpace(os.Getenv(exposeTokenKey))
	if token == "" {
		return fail(fmt.Errorf("no expose token: $%s is unset or empty — the relay carries no credential of its own", exposeTokenKey))
	}
	upstream := strings.TrimSpace(o.upstream)
	if upstream == "" {
		upstream = strings.TrimSpace(os.Getenv(exposeUpstreamKey))
	}
	if upstream == "" {
		return fail(fmt.Errorf("no upstream app route: pass --upstream or set $%s", exposeUpstreamKey))
	}
	expiresText := strings.TrimSpace(o.expires)
	if expiresText == "" {
		expiresText = strings.TrimSpace(os.Getenv(exposeExpiresKey))
	}
	var expires time.Time
	if expiresText != "" {
		t, err := time.Parse(time.RFC3339, expiresText)
		if err != nil {
			return fail(fmt.Errorf("expiry %q is not an RFC3339 instant", expiresText))
		}
		expires = t
	}
	now := env.now()
	if !expires.IsZero() && !now.Before(expires) {
		return fail(fmt.Errorf("the expose expired at %s — nothing served; re-expose it", expires.UTC().Format(time.RFC3339)))
	}
	ctx := context.Background()
	if env.relayDialCtx != nil {
		ctx = env.relayDialCtx()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The TTL cut is immediate: when the expiry passes, the relay stops
	// serving and exits non-zero rather than answering one more request.
	expired := make(chan struct{})
	if !expires.IsZero() {
		timer := time.AfterFunc(expires.Sub(now), func() {
			close(expired)
			cancel()
		})
		defer timer.Stop()
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %v", o.listen, err))
	}
	defer ln.Close()
	serveErr := make(chan error, 1)
	switch o.mode {
	case exposeKindHTTP:
		h, herr := relayHTTPHandler(upstream, token, o.pathPrefix)
		if herr != nil {
			return fail(herr)
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		go func() { serveErr <- srv.Serve(ln) }()
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
	case exposeKindTCP:
		go func() { serveErr <- relayTCPServe(ctx, ln, upstream, token) }()
	case exposeEgressKind:
		go func() { serveErr <- relayEgressServe(ctx, ln, upstream, token) }()
	case "":
		return fail(errors.New("--mode is required: http, tcp or egress"))
	default:
		return fail(fmt.Errorf("--mode %q: want http, tcp or egress (a git expose rides --mode http)", o.mode))
	}
	select {
	case <-expired:
		return fail(fmt.Errorf("the expose expired at %s — stopped serving", expires.UTC().Format(time.RFC3339)))
	case err := <-serveErr:
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return fail(err)
	}
}

// exposeHeaders are the two headers every relayed request carries: the
// expose-token guard Outpost checks, and the Bearer shape the satellite edge
// needs so it neither strips Authorization nor demands CSRF on a POST. The
// Bearer value is NEVER used for authentication — it is the same app-local
// token, shaped for the edge.
func exposeHeaders(token string) http.Header {
	h := http.Header{}
	h.Set(exposeTokenHeader, token)
	h.Set("Authorization", "Bearer "+token)
	return h
}

// relayHTTPHandler reverse-proxies to the app route, adding both headers and
// an optional path prefix. The prefix is how one relay fronts a sub-route of
// the app (the LLM door's /k/<door-peer>/).
func relayHTTPHandler(upstream, token, pathPrefix string) (http.Handler, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream %q is not an http(s) app route", upstream)
	}
	base := *u
	base.Path = joinURLPath(base.Path, pathPrefix)
	base.RawQuery, base.Fragment = "", ""
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&base)
			pr.Out.Host = base.Host
			for k, vs := range exposeHeaders(token) {
				pr.Out.Header[k] = vs
			}
		},
	}, nil
}

// joinURLPath joins two URL path segments with exactly one slash, keeping
// the trailing slash ReverseProxy.SetURL needs to append onto.
func joinURLPath(base, extra string) string {
	extra = strings.Trim(strings.TrimSpace(extra), "/")
	if extra == "" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + extra + "/"
}

// relayTCPServe accepts connections and bridges each one over a single
// WebSocket upgrade of the app route, carrying both headers. Outpost's app
// side byte-bridges the upgrade to the tcp target, so a connection is the
// websocket: no multiplexing, no framing of our own.
func relayTCPServe(ctx context.Context, ln net.Listener, upstream, token string) error {
	endpoint, err := tunnelWSScheme(strings.TrimSpace(upstream))
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go bridgeTCPOverWS(ctx, conn, endpoint, token)
	}
}

// bridgeTCPOverWS is one accepted connection's life: dial the app route as a
// websocket, then copy bytes both ways until either side ends.
func bridgeTCPOverWS(ctx context.Context, conn net.Conn, endpoint, token string) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: exposeHeaders(token)})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return
	}
	defer ws.CloseNow()
	bridgeWSConn(ctx, conn, ws)
}

// bridgeWSConn copies bytes both ways between a TCP connection and an open
// websocket until either side ends.
func bridgeWSConn(ctx context.Context, conn net.Conn, ws *websocket.Conn) {
	remote := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, remote)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// relayEgressServe listens as an HTTP CONNECT proxy on :3128. Each CONNECT
// host:port is bridged over one ?dst=host:port WebSocket upgrade of the
// upstream app route. Anything that is not a CONNECT is refused with 405;
// a WebSocket dial failure or an immediate policy close is refused with 403
// before any 200 is written.
func relayEgressServe(ctx context.Context, ln net.Listener, upstream, token string) error {
	wsBase, err := tunnelWSScheme(strings.TrimSpace(upstream))
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handleEgressConn(ctx, conn, wsBase, token)
	}
}

func handleEgressConn(ctx context.Context, conn net.Conn, wsBase, token string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), "CONNECT") {
		msg := "method not allowed: egress proxy handles CONNECT only; use https via CONNECT\n"
		fmt.Fprintf(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	hostport := strings.TrimSpace(req.Host)
	if hostport == "" {
		hostport = strings.TrimSpace(req.URL.Host)
	}
	if hostport == "" {
		hostport = strings.TrimSpace(req.RequestURI)
	}
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		msg := "bad CONNECT target: want host:port\n"
		fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	sep := "?"
	if strings.Contains(wsBase, "?") {
		sep = "&"
	}
	endpoint := wsBase + sep + "dst=" + url.QueryEscape(hostport)
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	forbidden := func() {
		msg := "forbidden\n"
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
	}
	// The Outpost app closes a disallowed dst with a policy status right
	// after the upgrade, which only a data-path read surfaces: Ping waits
	// for a pong the closed app never sends, while Read reports the close
	// frame at once. A timed-out or cancelled read closes the connection
	// it ran on, so the probe connection is sacrificial: a clean probe is
	// followed by a fresh bridge dial, and 200 is written only then.
	probe, resp, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPHeader: exposeHeaders(token)})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		forbidden()
		return
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, _, perr := probe.Read(probeCtx)
	probeCancel()
	_ = probe.CloseNow()
	if ctx.Err() != nil {
		return
	}
	if perr != nil && websocket.CloseStatus(perr) != websocket.StatusCode(-1) {
		forbidden()
		return
	}
	ws, resp, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPHeader: exposeHeaders(token)})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		forbidden()
		return
	}
	defer ws.CloseNow()
	fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	bridgeWSConn(ctx, conn, ws)
}
