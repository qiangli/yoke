package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/telemetry"
)

// Subscription worker states (Sprint 646 decision 5). Reported honestly:
// ok, missing, expired, plan-limit, offline. They are the same reasons the
// task wire contract uses when a worker cannot run.
type WorkerState string

const (
	// WorkerOK means the vendor CLI verified a subscription login.
	WorkerOK WorkerState = "ok"
	// WorkerMissing means the probe ran and found no subscription login.
	WorkerMissing WorkerState = "missing"
	// WorkerExpired means a login exists but is no longer valid
	// (expired, revoked, refresh failed): re-authentication is required.
	WorkerExpired WorkerState = "expired"
	// WorkerPlanLimit means the plan's usage/quota window is exhausted.
	WorkerPlanLimit WorkerState = "plan-limit"
	// WorkerOffline means the check could not run at all: the binary is
	// not installed, the tool is unknown, or the probe failed to execute.
	WorkerOffline WorkerState = "offline"
)

// SubscriptionWorkers are the vendor CLIs that run on an outpost under the
// owner's subscription (Sprint 646 decisions 4-5). Each authenticates with its
// vendor's documented headless method only; there is never an API-key
// fallback.
var SubscriptionWorkers = []string{"claude", "codex", "agy", "muse"}

// WorkerReport is one subscription worker's credential state.
type WorkerReport struct {
	Name   string      `json:"name"`
	State  WorkerState `json:"state"`
	Detail string      `json:"detail,omitempty"`
}

// WorkerCheck is the raw outcome of running a tool's auth check.
type WorkerCheck struct {
	MissingBinary bool
	TimedOut      bool
	ExitCode      int
	Output        string // combined stdout+stderr, bounded
}

// workerExpired matches output showing a login that stopped being valid.
var workerExpired = regexp.MustCompile(`(?i)(expir(?:ed|ing|y)|failed to refresh|reauth|re-auth|invalid token|token (is )?invalid|revoked|session expired|credentials? expired|log ?in again|re-?login required)`)

// workerPlanLimit matches output showing an exhausted plan window. Per-tool
// usage_signals are checked first; this is the vendor-neutral backstop.
var workerPlanLimit = regexp.MustCompile(`(?i)(usage limit|rate ?limit|quota|plan.?limit|limit (reached|exceeded|exhausted)|too many requests|overloaded|billing_?error|payment required|402|out of credits|credit balance)`)

// workerMissing matches output showing no usable login. It deliberately has
// no bare "signed in" alternative: a successful login prints no such plea.
var workerMissing = regexp.MustCompile(`(?i)(not logged in|not signed in|logged out|no .*log ?in|not authenticated|please (log ?in|authenticate)|sign[ -]in required|waiting for approval|page to sign in|no subscription|not subscribed|missing credential|unauthorized|401|"loggedIn":\s*false)`)

// CheckAuthOutput reports whether a check run verified the subscription
// login: exit 0 with every declared ok_match regex matching the combined
// output. An empty ok_match list means the exit code alone decides (for
// tools whose vendor documents no status spelling).
func CheckAuthOutput(t Tool, exitCode int, output string) bool {
	if exitCode != 0 {
		return false
	}
	for _, m := range t.CLI.Launch.Auth.OkMatch {
		re, err := regexp.Compile(m)
		if err != nil || !re.MatchString(output) {
			return false
		}
	}
	return true
}

// planLimitSignal reports whether any of the tool's declared usage signals
// fires on the check output (same matcher vocabulary as the launch path).
func planLimitSignal(t Tool, output string) bool {
	for _, s := range t.UsageSignals {
		if strings.TrimSpace(s.Match) == "" {
			continue
		}
		re, err := regexp.Compile(s.Match)
		if err != nil {
			continue
		}
		if re.MatchString(output) {
			return true
		}
	}
	return false
}

// ClassifyWorker maps a finished check to its honest state.
func ClassifyWorker(t Tool, res WorkerCheck) WorkerState {
	if res.MissingBinary {
		return WorkerOffline
	}
	if planLimitSignal(t, res.Output) || workerPlanLimit.MatchString(res.Output) {
		return WorkerPlanLimit
	}
	if workerExpired.MatchString(res.Output) {
		return WorkerExpired
	}
	if CheckAuthOutput(t, res.ExitCode, res.Output) {
		return WorkerOK
	}
	if res.TimedOut && strings.TrimSpace(res.Output) == "" {
		return WorkerOffline
	}
	return WorkerMissing
}

// AuthCheckArgv renders the probe argv for a tool: the lane args pinning the
// subscription lane, then the declared check fragment. Empty when the tool
// declares no subscription check — the caller fails closed.
func AuthCheckArgv(t Tool) []string {
	check := strings.Fields(t.CLI.Launch.Auth.CheckExec)
	if len(check) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.CLI.Launch.Auth.LaneArgs)+len(check))
	out = append(out, t.CLI.Launch.Auth.LaneArgs...)
	return append(out, check...)
}

// workerConfigRoot is the bashy-owned home for per-worker config dirs,
// beside the other ~/.bashy state (sessions, logs, chat). A worker's config
// dir isolates its account and session state from the operator's interactive
// login on the same host.
func workerConfigRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("fleet: no home directory for worker config")
	}
	return filepath.Join(home, ".bashy", "workers"), nil
}

// WorkerConfigDir ensures the stable per-worker config dir for a tool and
// returns its path. Stable, not per-run: the cached login (device-code
// cache, persisted sign-in) must survive across delegations.
func WorkerConfigDir(toolName string) (string, error) {
	root, err := workerConfigRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, strings.ToLower(toolName), "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("fleet: worker config dir: %w", err)
	}
	return dir, nil
}

// workerLoginFiles mirrors weave's booth login allowlist: only these files
// are ever seeded from the operator's config into a worker dir. Values are
// copied, never printed or logged.
var workerLoginFiles = []string{"auth.json", ".credentials.json", "oauth_creds.json", "google_accounts.json", "antigravity-oauth-token"}

// seedWorkerDir copies the login-file allowlist from the operator's config
// source into the worker dir, skipping absent files. Best-effort: a missing
// source means the operator has not signed in, which the check reports as
// missing rather than failing the launch setup.
func seedWorkerDir(source, dest string) {
	for _, name := range workerLoginFiles {
		b, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dest, name), b, 0o600)
	}
}

// expandConfigSource expands one ${VAR:-default} / $VAR / ~/ path template as
// the tool integration detect fields spell them.
func expandConfigSource(tmpl string) string {
	tmpl = strings.TrimSpace(tmpl)
	if strings.HasPrefix(tmpl, "${") {
		end := strings.IndexByte(tmpl, '}')
		if end < 0 {
			return ""
		}
		inner := tmpl[2:end]
		rest := tmpl[end+1:]
		name, def, _ := strings.Cut(inner, ":-")
		if v := os.Getenv(name); v != "" {
			return v + rest
		}
		return expandConfigSource(def + rest)
	}
	if strings.HasPrefix(tmpl, "$") {
		name := strings.TrimLeft(tmpl[1:], "{")
		name = strings.TrimRight(name, "}")
		if i := strings.IndexAny(name, "/"); i >= 0 {
			name = name[:i]
		}
		if v := os.Getenv(name); v != "" {
			return v
		}
		return ""
	}
	if strings.HasPrefix(tmpl, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, tmpl[2:])
		}
		return ""
	}
	return tmpl
}

// detectBaseVar names the environment variable the detect template relocates
// through: the ${VAR} it opens with, or HOME for a ~/ path. Empty when the
// template names no variable.
func detectBaseVar(tmpl string) string {
	tmpl = strings.TrimSpace(tmpl)
	if strings.HasPrefix(tmpl, "${") {
		end := strings.IndexByte(tmpl, '}')
		if end < 0 {
			return ""
		}
		name, _, _ := strings.Cut(tmpl[2:end], ":")
		return strings.TrimSpace(name)
	}
	if strings.HasPrefix(tmpl, "$") {
		name := strings.TrimLeft(tmpl[1:], "{")
		name = strings.TrimRight(name, "}")
		if i := strings.IndexAny(name, "/"); i >= 0 {
			name = name[:i]
		}
		return name
	}
	if strings.HasPrefix(tmpl, "~/") {
		return "HOME"
	}
	return ""
}

// PrepareWorkerEnv builds the subscription-worker environment additions from
// the tool declaration. rawParent is the launcher's own environment
// (pre-scrub): the token resolves through it via secrets.ResolveAgentKey —
// the host secrets.map binding, then the vault — exactly like `bashy secret
// env`. child is the already-scrubbed child environment to extend.
//
// The worker never shares the operator's interactive login: every tool gets
// its own config home under ~/.bashy/workers, relocated through the same
// variable the vendor CLI reads (mirroring weave booths):
//
//   - A tool declaring auth.token_ref (claude) is env-authenticated: the
//     vault OAuth token lands via secrets.PreserveEnvNames, and the dir
//     stays fresh. An unresolvable ref fails closed — never an API key.
//   - A tool whose detect template opens with its own config_dir_env
//     (codex: ${CODEX_HOME:-~/.codex}) is relocated directly: the dir IS the
//     config location, seeded with the login-file allowlist (device-code
//     cache).
//   - Otherwise config_dir_env is a redirect marker and the vendor reads
//     through a base variable (agy: HOME/.gemini, muse:
//     XDG_CONFIG_HOME/muse): the base is pointed at the worker home and the
//     config is seeded beneath it (persisted sign-in, subscription
//     credential).
func PrepareWorkerEnv(rawParent, child []string, t Tool) ([]string, error) {
	dirEnv := strings.TrimSpace(t.CLI.Launch.ConfigDirEnv)
	if dirEnv == "" {
		return child, nil
	}
	root, err := workerConfigRoot()
	if err != nil {
		return nil, err
	}
	home := filepath.Join(root, strings.ToLower(t.Name))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("fleet: worker home: %w", err)
	}
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fleet: worker config dir: %w", err)
	}
	if ref := strings.TrimSpace(t.CLI.Launch.Auth.TokenRef); ref != "" {
		kv, ok := secrets.ResolveAgentKey(rawParent, ref)
		if !ok {
			return nil, fmt.Errorf("fleet: worker %s has no subscription credential for vault ref %q (sign in with the vendor CLI, never an API key)", t.Name, ref)
		}
		child = setWorkerEnv(child, dirEnv, dir)
		name, _, _ := strings.Cut(kv, "=")
		return secrets.PreserveEnvNames(child, []string{kv}, []string{name}), nil
	}
	detect := strings.TrimSpace(t.Integration.Detect)
	base := detectBaseVar(detect)
	src := expandConfigSource(detect)
	if base != "" && base != dirEnv {
		// Marker redirect: the vendor reads through the base variable,
		// so the base moves to the worker home, not just the marker.
		child = setWorkerEnv(child, base, dir)
		if src != "" {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Base(src)), 0o700); err != nil {
				return nil, fmt.Errorf("fleet: worker config dir: %w", err)
			}
			seedWorkerDir(src, filepath.Join(dir, filepath.Base(src)))
		}
		child = setWorkerEnv(child, dirEnv, filepath.Join(dir, filepath.Base(src)))
		return child, nil
	}
	// Direct redirect: config_dir_env names the config location itself.
	child = setWorkerEnv(child, dirEnv, dir)
	if src != "" {
		seedWorkerDir(src, dir)
	}
	return child, nil
}

// ProbeWorker runs one tool's declared subscription check in the same
// worker environment a delegation would see (scrubbed launcher env plus the
// per-worker config dir and vault token), then classifies the outcome.
func ProbeWorker(ctx context.Context, t Tool, timeout time.Duration) WorkerReport {
	rep := WorkerReport{Name: t.Name}
	argv := AuthCheckArgv(t)
	if len(argv) == 0 {
		rep.State, rep.Detail = WorkerOffline, "no subscription check declared"
		return rep
	}
	bin := t.CLI.Binary
	if bin == "" {
		bin = t.Name
	}
	child := secrets.ScrubAgentEnv(os.Environ())
	prepared, err := PrepareWorkerEnv(os.Environ(), child, t)
	if err != nil {
		rep.State, rep.Detail = WorkerMissing, err.Error()
		return rep
	}
	res := RunWorkerCheck(ctx, bin, argv, prepared, timeout)
	rep.State = ClassifyWorker(t, res)
	switch rep.State {
	case WorkerOK:
		rep.Detail = "subscription login verified"
	case WorkerMissing:
		rep.Detail = "no subscription login; sign in with the vendor CLI (" + authHintFor(t) + ")"
	case WorkerExpired:
		rep.Detail = "subscription login expired; re-authenticate with the vendor CLI"
	case WorkerPlanLimit:
		rep.Detail = "plan usage window exhausted; wait for reset or upgrade the seat"
	case WorkerOffline:
		if !res.MissingBinary && res.TimedOut {
			rep.Detail = "check timed out"
		} else if res.Output != "" {
			rep.Detail = "check did not run: " + firstLine(res.Output)
		} else {
			rep.Detail = "vendor CLI is not installed"
		}
	}
	return rep
}

func authHintFor(t Tool) string {
	if h := strings.TrimSpace(t.CLI.Launch.AuthHint); h != "" {
		if i := strings.IndexByte(h, '\n'); i >= 0 {
			return strings.TrimSpace(h[:i])
		}
		return h
	}
	return "see the tool's auth_hint"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ProbeWorkers checks every named subscription worker concurrently and
// returns the reports in name order.
func ProbeWorkers(ctx context.Context, c *Catalog, names []string, timeout time.Duration) []WorkerReport {
	out := make([]WorkerReport, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			t, ok := c.Tool(name)
			if !ok {
				out[i] = WorkerReport{Name: name, State: WorkerOffline, Detail: "not in the catalog"}
				return
			}
			out[i] = ProbeWorker(ctx, t, timeout)
		}(i, name)
	}
	wg.Wait()
	return out
}

func setWorkerEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// RunWorkerCheck executes one auth check with a bounded wait and bounded,
// withheld output. Stdin is closed: a check never interacts, so a signed-out
// CLI that would prompt (muse login's device flow) is observed by its prompt
// text and classified missing instead of hanging the caller.
func RunWorkerCheck(ctx context.Context, binary string, argv, env []string, timeout time.Duration) WorkerCheck {
	path, err := exec.LookPath(binary)
	if err != nil {
		return WorkerCheck{MissingBinary: true, ExitCode: 127, Output: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Env = env
	cmd.Stdin = nil
	var out boundedWorkerOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return WorkerCheck{TimedOut: true, ExitCode: 124, Output: string(out.data)}
		}
		return WorkerCheck{ExitCode: 127, Output: string(out.data) + err.Error()}
	}
	err = cmd.Wait()
	res := WorkerCheck{Output: string(out.data)}
	if ctx.Err() != nil {
		res.TimedOut = true
		res.ExitCode = 124
		return res
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exit.ExitCode()
		} else {
			res.ExitCode = 1
		}
	}
	return res
}

// boundedWorkerOutput caps check output like the delegate auth probe: status
// errors can contain account data, so output is classified, never returned.
type boundedWorkerOutput struct{ data []byte }

func (b *boundedWorkerOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 16384 - len(b.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

// WorkerAlert is one probe finding persisted for the owner. The daily probe
// appends these to alerts.jsonl (bounded, last 200) and emits them on
// stderr (cron mails it) and OTel; Sprint 620 comms deliver them to the
// owner from there. No credential values are ever recorded.
type WorkerAlert struct {
	At     time.Time   `json:"at"`
	Host   string      `json:"host"`
	Worker string      `json:"worker"`
	State  WorkerState `json:"state"`
	Detail string      `json:"detail"`
}

// recordWorkerAlerts appends one alert per non-ok report to the bounded
// alert log and returns the appended records.
func recordWorkerAlerts(reports []WorkerReport) ([]WorkerAlert, error) {
	root, err := workerConfigRoot()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	var alerts []WorkerAlert
	for _, r := range reports {
		if r.State == WorkerOK {
			continue
		}
		alerts = append(alerts, WorkerAlert{At: time.Now().UTC(), Host: host, Worker: r.Name, State: r.State, Detail: r.Detail})
	}
	if len(alerts) == 0 {
		return nil, nil
	}
	path := filepath.Join(root, "alerts.jsonl")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(b), "\n")
	}
	for _, a := range alerts {
		b, _ := json.Marshal(a)
		lines = append(lines, string(b))
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	return alerts, nil
}

// newWorkersCmd builds `bashy agents workers`: the honest credential state
// of every subscription worker, for the operator and for the daily probe.
//
// States: ok (verified subscription login), missing (no login), expired
// (login invalid), plan-limit (quota window exhausted), offline (the check
// could not run). A check never signs in and never spends: it runs the
// vendor CLI's own status spelling in a scrubbed environment and classifies
// the output. No API-key fallback exists anywhere in this path.
func newWorkersCmd(opts []Option) *cobra.Command {
	var asJSON bool
	var probe bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "workers [name...]",
		Short: "Report subscription worker credential states",
		Long: `Report the credential state of the subscription workers (claude, codex, agy, muse).

States are ok, missing, expired, plan-limit, or offline. Each check runs the
vendor CLI's own documented status spelling in a scrubbed environment and
classifies the output; a check never signs in and never spends, and no
API-key fallback exists.

--probe is the daily-probe mode: non-ok workers are recorded in
~/.bashy/workers/alerts.jsonl (bounded, last 200) for Sprint 620 comms
delivery, emitted on stderr for cron mail, and counted in OTel
(workers.probe). It exits nonzero when any worker needs attention.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			names := append([]string(nil), SubscriptionWorkers...)
			if len(args) > 0 {
				names = args
			}
			c := New(opts...)
			reports := ProbeWorkers(cmd.Context(), c, names, timeout)
			if asJSON {
				b, _ := json.Marshal(reports)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "WORKER\tSTATE\tDETAIL")
				for _, r := range reports {
					fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, r.State, r.Detail)
				}
				_ = w.Flush()
			}
			if !probe {
				return nil
			}
			alerts, err := recordWorkerAlerts(reports)
			if err != nil {
				return err
			}
			bad := 0
			var summary []string
			for _, r := range reports {
				if r.State != WorkerOK {
					bad++
					summary = append(summary, r.Name+": "+string(r.State))
				}
			}
			telemetry.BoundHit(cmd.Context(), "workers.probe", int64(bad), int64(len(reports)), "subscription worker probe: "+strings.Join(summary, "; "))
			for _, a := range alerts {
				fmt.Fprintf(cmd.ErrOrStderr(), "ALERT worker=%s state=%s detail=%s\n", a.Worker, a.State, a.Detail)
			}
			if bad > 0 {
				return fmt.Errorf("workers probe: %d of %d workers need attention (%s)", bad, len(reports), strings.Join(summary, ", "))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON records")
	c.Flags().BoolVar(&probe, "probe", false, "daily-probe mode: record alerts, emit telemetry, exit nonzero on non-ok")
	c.Flags().DurationVar(&timeout, "probe-timeout", 10*time.Second, "bound each vendor check")
	return c
}
