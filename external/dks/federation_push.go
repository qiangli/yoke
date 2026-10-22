// Push-mode satellite federation lifecycle.
//
// `bashy dks federation join|leave|status --mode push` federates two
// satellite-hosted (embedded) DKS planes through the satellites' own HTTPS
// API: it never goes through Outpost. Pull mode (the default, no --mode
// flag) keeps the existing behaviour of exec'ing
// `outpost cluster federation …` for peer planes.
//
// Shared contract (see docs/dks-federation-karmada.md §10.1; the satellite
// API surface itself is owned by the cloudbox run):
//
//	POST <sat>/api/v1/tokens/exchange            owner Bearer -> short-lived satellite token
//	POST <host>/api/cluster/install/karmada      ensure the karmada builtin (certSANs cover karmada.<sat-host>)
//	GET  <host>/api/cluster/karmada/status       Karmada readiness probe (falls back to the kubeconfig endpoint)
//	GET  <host>/api/cluster/karmada/kubeconfig   admin kubeconfig, server https://karmada.<sat-host>
//	POST <member>/api/cluster/federation/member-credential
//	                                            create SA dhnt-karmada-member + cluster-admin binding + token Secret
//	DELETE <member>/api/cluster/federation/member-credential
//	                                            revoke exactly those
//
// The member registers as a Karmada Cluster in Push mode over the Karmada
// apiserver HTTPS API (Secret holding the member kubeconfig + Cluster
// object with spec.syncMode Push), using only the standard library plus
// gopkg.in/yaml.v3 for kubeconfig parsing — no karmada/client-go
// dependency (kept permissive per the story).
//
// Exactly what was created on both sides lands in a baseline record at
// $BASHY_HOME/dks/federation/<host>/<cluster>.json (0600, resource refs
// only — never tokens or kubeconfigs). `leave` removes only what the
// baseline names and refuses when there is no baseline, so a pre-existing
// karmada-system namespace or Cluster is never adopted or deleted.
// Karmada itself stays installed unless `leave --uninstall` is given AND
// this tool performed the install.
package dks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/fleet"
)

// FederationPushScope is the default token-exchange scope gating the
// satellite federation endpoints. The cloudbox run owns the gate; if it
// settles on a different name, pass --scope (repeatable / comma-separated)
// or set $BASHY_FEDERATION_SCOPE instead of rebuilding.
const FederationPushScope = "cluster:federation"

// Satellite endpoint paths consumed by push mode. The install path is
// pre-existing; kubeconfig and member-credential are new in the shared
// contract. The status and uninstall paths are client conventions the
// server may or may not implement yet — both degrade gracefully (see
// waitKarmadaReady and uninstallKarmada).
const (
	pushPathInstall    = "/api/cluster/install/karmada"
	pushPathStatus     = "/api/cluster/karmada/status"
	pushPathKubeconfig = "/api/cluster/karmada/kubeconfig"
	pushPathMemberCred = "/api/cluster/federation/member-credential"
	pushExchangeTTL    = "60m"
	pushWaitDefault    = "15m"
	pushMemberSA       = "dhnt-karmada-member"
	pushKarmadaNS      = "karmada-system"
	pushRecordVersion  = 1
	pushBaselinePerm   = 0o600
	pushMaxBody        = 1 << 20
	pushKubeconfigKey  = "kubeconfig"
	pushSyncMode       = "Push"
	pushClusterAPIVer  = "cluster.karmada.io/v1alpha1"
)

// pushStatusInterval paces the Karmada readiness poll. A variable so tests
// can shrink it without touching the --wait timeout semantics.
var pushStatusInterval = 10 * time.Second

// pushSatelliteHTTP builds satellite API clients. A variable so tests can
// observe or substitute transport behaviour.
var pushSatelliteHTTP = func() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

// pushOptions are the parsed --mode push flags shared by join, leave and
// status. Only the verbs that need a field read it.
type pushOptions struct {
	hostSatellite   string
	memberSatellite string
	name            string
	dryRun          bool
	tokenOverride   string
	scopes          []string
	ttl             string
	wait            time.Duration
	uninstall       bool
	asJSON          bool
}

// hasModePush reports whether the pass-through args select push mode:
// --mode push or --mode=push. Any other --mode value (or none) keeps the
// existing Outpost passthrough.
func hasModePush(args []string) bool {
	for i, a := range args {
		if a == "--mode=push" {
			return true
		}
		if a == "--mode" && i+1 < len(args) && args[i+1] == "push" {
			return true
		}
	}
	return false
}

// parsePushFlags parses the --mode push flag set for one subcommand. It
// rejects unknown flags loudly: push mode owns its surface and must not
// silently forward Outpost pull-mode flags to satellite endpoints.
func parsePushFlags(sub string, args []string) (pushOptions, error) {
	var o pushOptions
	var scopeVal []string
	var waitStr string
	fs := pflag.NewFlagSet("federation "+sub, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.hostSatellite, "host-satellite", "", "host satellite base URL (required)")
	fs.StringVar(&o.memberSatellite, "member-satellite", "", "member satellite base URL (required for join/leave)")
	fs.StringVar(&o.name, "name", "", "Karmada Cluster name (default derived from the member satellite host)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan with secrets redacted; make zero network calls")
	fs.StringVar(&o.tokenOverride, "token", "", "owner Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY)")
	fs.StringSliceVar(&scopeVal, "scope", nil, "token-exchange scope (repeatable or comma-separated; default "+FederationPushScope+")")
	fs.StringVar(&o.ttl, "ttl", pushExchangeTTL, "token-exchange TTL (capped at 1h by the satellite)")
	fs.StringVar(&waitStr, "wait", pushWaitDefault, "how long to wait for Karmada Ready on join (Go duration)")
	fs.BoolVar(&o.uninstall, "uninstall", false, "leave: also uninstall Karmada, but only if this tool installed it")
	fs.BoolVar(&o.asJSON, "json", false, "status: emit machine-readable output")
	// Accept and ignore the --mode selector itself so the flag set round-trips.
	var mode string
	fs.StringVar(&mode, "mode", "", "must be push (accepted and ignored here)")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("dks federation %s --mode push: %v", sub, err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return o, fmt.Errorf("dks federation %s --mode push: unexpected argument %q", sub, rest[0])
	}
	if mode != "" && mode != "push" {
		return o, fmt.Errorf("dks federation %s: --mode %q reached the push parser, want push", sub, mode)
	}
	if o.ttl == "" {
		o.ttl = pushExchangeTTL
	}
	for _, s := range scopeVal {
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				o.scopes = append(o.scopes, p)
			}
		}
	}
	if len(o.scopes) == 0 {
		if env := strings.TrimSpace(os.Getenv("BASHY_FEDERATION_SCOPE")); env != "" {
			for _, part := range strings.Split(env, ",") {
				if p := strings.TrimSpace(part); p != "" {
					o.scopes = append(o.scopes, p)
				}
			}
		}
	}
	if len(o.scopes) == 0 {
		o.scopes = []string{FederationPushScope}
	}
	wait, err := time.ParseDuration(strings.TrimSpace(waitStr))
	if err != nil || wait <= 0 {
		return o, fmt.Errorf("dks federation %s --mode push: bad --wait %q (Go duration > 0)", sub, waitStr)
	}
	o.wait = wait
	o.hostSatellite = strings.TrimSpace(o.hostSatellite)
	o.memberSatellite = strings.TrimSpace(o.memberSatellite)
	o.name = strings.TrimSpace(o.name)
	if o.hostSatellite == "" {
		return o, fmt.Errorf("dks federation %s --mode push needs --host-satellite URL", sub)
	}
	if sub != "status" && o.memberSatellite == "" && o.name == "" {
		return o, fmt.Errorf("dks federation %s --mode push needs --member-satellite URL (or --name of a recorded member)", sub)
	}
	if _, _, err := pushResolveToken(o.tokenOverride); err != nil {
		return o, err
	}
	return o, nil
}

// pushResolveToken walks the owner-Bearer [REDACTED] --token > $BASHY_FLEET_TOKEN >
// $BASHY_API_KEY — and reports where the winner came from for redacted
// dry-run display. The token value itself is never rendered by callers.
func pushResolveToken(override string) (token, source string, err error) {
	if override != "" {
		return override, "--token", nil
	}
	if v := os.Getenv("BASHY_FLEET_TOKEN"); v != "" {
		return v, "$BASHY_FLEET_TOKEN", nil
	}
	if v := os.Getenv("BASHY_API_KEY"); v != "" {
		return v, "$BASHY_API_KEY", nil
	}
	return "", "", fmt.Errorf("dks federation --mode push: no owner Bearer [REDACTED] $BASHY_FLEET_TOKEN or pass --token)")
}

// pushSatellite is one satellite's API surface with an exchanged token.
type pushSatellite struct {
	baseURL   string
	token     string // exchanged satellite token; never printed
	http      *http.Client
	exchanged bool
}

// pushDial exchanges the owner Bearer [REDACTED] one satellite and returns an authenticated
// caller. The owner Bearer [REDACTED] used for exactly one exchange call and is never
// stored on the returned value.
func pushDial(satelliteURL, ownerToken string, scopes []string, ttl string) (*pushSatellite, error) {
	base := strings.TrimRight(strings.TrimSpace(satelliteURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	client := fleet.SatelliteClient{BaseURL: base, Token: ownerToken, HTTP: pushSatelliteHTTP()}
	exchanged, err := client.ExchangeToken(scopes, ttl)
	if err != nil {
		return nil, pushExchangeError(base, err)
	}
	return &pushSatellite{baseURL: base, token: exchanged, http: pushSatelliteHTTP(), exchanged: true}, nil
}

// pushExchangeError rewords an exchange failure without embedding either
// Bearer [REDACTED] The fleet client already avoids token values; this only adds the
// satellite-independent hint about the scope gate.
func pushExchangeError(base string, err error) error {
	msg := err.Error()
	if strings.Contains(msg, "403") || strings.Contains(msg, "Forbidden") || strings.Contains(msg, "lacks") {
		return fmt.Errorf("dks federation --mode push: token exchange refused on %s: owner token lacks scope (need one of the push scopes; see --scope, default %s)", pushHostOf(base), FederationPushScope)
	}
	return fmt.Errorf("dks federation --mode push: token exchange on %s failed: %v", pushHostOf(base), redactTokenValues(msg))
}

// redactTokenValues is a last-resort scrubber for error text that may have
// passed through a server body echo. It cannot know token values, so the
// rule is structural: callers must never interpolate tokens into errors,
// and this keeps the raw bodies the provisionAPIError path already trimmed.
func redactTokenValues(s string) string {
	return s
}

// do performs one authenticated satellite call. 2xx returns the bounded
// body; anything else is a trimmed, secret-free error naming method+path.
func (s *pushSatellite) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("dks federation --mode push: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, pushMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("dks federation --mode push: %s %s: read: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("dks federation --mode push: %s %s: %d %s", method, path, resp.StatusCode, pushTrimBody(raw))
	}
	return raw, resp.StatusCode, nil
}

// pushTrimBody renders a refusal body in one line, capped, without secrets:
// the satellite envelope is {code, error, ...} and these endpoints never
// return tokens in refusal bodies.
func pushTrimBody(raw []byte) string {
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return "empty refusal body"
	}
	var env struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && (env.Error != "" || env.Code != "") {
		if env.Error != "" {
			msg = env.Error
			if env.Code != "" {
				msg = env.Code + ": " + msg
			}
		} else {
			msg = env.Code
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// pushHostOf renders a satellite base URL as its bare host for messages and
// record paths. It never emits credentials: URLs carry none in this flow.
func pushHostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return base
}

var pushSanitizeRe = regexp.MustCompile(`[^a-z0-9-]+`)

// pushSanitize maps a hostname to [a-z0-9-]: lowercase, dots and other
// separators become dashes, edges trimmed. Empty output falls back to
// "satellite" so record paths are always spellable.
func pushSanitize(host string) string {
	s := pushSanitizeRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(host)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "satellite"
	}
	return s
}

// defaultClusterName derives the Karmada Cluster name from the member
// satellite host: first DNS label sanitised, plus the port when it is not
// a default HTTPS/HTTP port (keeps httptest members distinct).
func defaultClusterName(memberSatellite string) string {
	base := strings.TrimRight(strings.TrimSpace(memberSatellite), "/")
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "member"
	}
	label := u.Hostname()
	if i := strings.Index(label, "."); i > 0 {
		label = label[:i]
	}
	name := pushSanitize(label)
	if p := u.Port(); p != "" && p != "443" && p != "80" {
		name += "-" + p
	}
	return name
}

// federationRecordDir is $BASHY_HOME/dks/federation, falling back to
// ~/.bashy/dks/federation when BASHY_HOME is unset.
func federationRecordDir() string {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "dks", "federation")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".bashy", "dks", "federation")
	}
	return filepath.Join(".bashy", "dks", "federation")
}

// pushRecordPath is the baseline file for one (host, cluster) pair.
func pushRecordPath(hostSatellite, clusterName string) string {
	return filepath.Join(federationRecordDir(), pushSanitize(pushHostOf(hostSatellite)), clusterName+".json")
}

// pushBaseline is the record of exactly what one push join created. Refs
// are resource references only — tokens and kubeconfigs are re-fetched at
// leave time through a fresh exchange, never kept at rest.
type pushBaseline struct {
	Version          int      `json:"version"`
	HostSatellite    string   `json:"hostSatellite"`
	MemberSatellite  string   `json:"memberSatellite"`
	ClusterName      string   `json:"clusterName"`
	KarmadaInstalled bool     `json:"karmadaInstalledByUs"`
	HostCreated      []string `json:"hostCreated"`
	MemberCreated    []string `json:"memberCreated"`
	// MemberReceipt is the member-credential "created" receipt exactly as
	// the satellite returned it (refs with UIDs, no secrets). Leave sends it
	// back verbatim: the satellite revokes only objects whose UIDs match.
	MemberReceipt json.RawMessage `json:"memberReceipt,omitempty"`
	MemberServer  string          `json:"memberServer,omitempty"`
	KarmadaServer string          `json:"karmadaServer,omitempty"`
	CreatedAt     string          `json:"createdAt"`
}

func pushWriteBaseline(rec pushBaseline) (string, error) {
	path := pushRecordPath(rec.HostSatellite, rec.ClusterName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("dks federation --mode push: create record dir: %w", err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, pushBaselinePerm); err != nil {
		return "", fmt.Errorf("dks federation --mode push: write baseline %s: %w", path, err)
	}
	_ = os.Chmod(path, pushBaselinePerm)
	return path, nil
}

func pushReadBaseline(hostSatellite, clusterName string) (pushBaseline, string, error) {
	var rec pushBaseline
	path := pushRecordPath(hostSatellite, clusterName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return rec, "", err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, path, fmt.Errorf("dks federation --mode push: decode baseline %s: %w", path, err)
	}
	return rec, path, nil
}

// pushFindBaseline locates the record for leave: --name wins; otherwise the
// name derives from --member-satellite; otherwise a host dir holding exactly
// one record resolves it, and an ambiguous dir is an error (never a guess).
func pushFindBaseline(o pushOptions) (pushBaseline, string, error) {
	if o.name != "" {
		rec, path, err := pushReadBaseline(o.hostSatellite, o.name)
		if err != nil {
			return rec, "", fmt.Errorf("dks federation leave --mode push: no baseline for member %q on host %s (joined with --mode push?)", o.name, pushHostOf(o.hostSatellite))
		}
		return rec, path, nil
	}
	if o.memberSatellite != "" {
		name := defaultClusterName(o.memberSatellite)
		rec, path, err := pushReadBaseline(o.hostSatellite, name)
		if err != nil {
			return rec, "", fmt.Errorf("dks federation leave --mode push: no baseline for member satellite %s (cluster %q) on host %s", pushHostOf(o.memberSatellite), name, pushHostOf(o.hostSatellite))
		}
		return rec, path, nil
	}
	dir := filepath.Join(federationRecordDir(), pushSanitize(pushHostOf(o.hostSatellite)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pushBaseline{}, "", fmt.Errorf("dks federation leave --mode push: no baseline on host %s (joined with --mode push?)", pushHostOf(o.hostSatellite))
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	if len(names) != 1 {
		return pushBaseline{}, "", fmt.Errorf("dks federation leave --mode push: %d recorded members on host %s, pass --name to choose", len(names), pushHostOf(o.hostSatellite))
	}
	rec, path, err := pushReadBaseline(o.hostSatellite, names[0])
	if err != nil {
		return rec, "", err
	}
	return rec, path, nil
}

// runPushMode is the --mode push entry for join, leave and status. Every
// other subcommand (or any other --mode value) never reaches here.
func runPushMode(ctx context.Context, out io.Writer, sub string, args []string) error {
	o, err := parsePushFlags(sub, args)
	if err != nil {
		return err
	}
	switch sub {
	case "join":
		return pushJoin(ctx, out, o)
	case "leave":
		return pushLeave(ctx, out, o)
	case "status":
		return pushStatus(ctx, out, o)
	default:
		return fmt.Errorf("dks federation %s has no --mode push path (push mode supports join, leave, status)", sub)
	}
}

// pushDryRunToken renders the token source without the value.
func pushDryRunToken(source string) string {
	if source == "--token" {
		return "--token [redacted]"
	}
	return source + " [redacted]"
}
