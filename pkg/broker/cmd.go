package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/resources"
)

// EngineArgv returns the command that starts the raw local engine (bashy
// sets it to [<bashy> ollama serve]; the child sees EngineModeEnv=1 and runs
// Ollama itself). Nil = the door serves no local models. A package variable,
// not os.Executable, so a test binary never re-executes itself.
var EngineArgv func() []string

// SelfArgv returns the command that runs this binary's `llm serve`, for
// `llm up` (bashy sets it). Nil = `llm up` is unavailable.
var SelfArgv func() []string

// DefaultEngineContext is the context length the engine loads models with
// unless OLLAMA_CONTEXT_LENGTH says otherwise: explicit, so a local identity
// always records it.
const DefaultEngineContext = 8192

// DoorOptions configures RunDoor.
type DoorOptions struct {
	Bind     string // loopback (default) | lan
	Port     int
	Policy   string
	Prewarm  bool
	NoEngine bool
	NoCLI    bool
	// MaxInFlight overrides the policy file's max_in_flight default: the
	// per-principal in-flight cap for fleet agents. Zero falls back to
	// CLIGW_MAX_IN_FLIGHT, then the policy file.
	MaxInFlight int
	Out         io.Writer
}

// cliAdapter makes a cligw server a CLIBackend.
type cliAdapter struct {
	s        *cligw.Server
	versions *toolVersions
}

func (a cliAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.s.Handler().ServeHTTP(w, r) }
func (a cliAdapter) Token() string                                    { return a.s.Token() }
func (a cliAdapter) ResolveAgent(ctx context.Context, model, filter string) (AgentInfo, error) {
	ag, err := a.s.ResolveAgent(ctx, model, filter)
	if err != nil {
		return AgentInfo{}, err
	}
	return a.info(ag), nil
}

// LookupAgent implements AgentLookup over cligw's live catalog.
func (a cliAdapter) LookupAgent(name string) (AgentInfo, bool) {
	ag, ok := a.s.Agent(name)
	if !ok {
		return AgentInfo{}, false
	}
	return a.info(ag), true
}

// DialSticky implements StickyDialer over cligw's session registry: one warm
// CLI per bind=worker/reset=none binding. Tools that cannot hold a session
// (and a full room) come back as cligw errors the broker maps to 501 and 429.
func (a cliAdapter) DialSticky(ctx context.Context, agent string) (stickyWorker, error) {
	sess, err := a.s.DialSticky(ctx, agent)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (a cliAdapter) info(ag cligw.Agent) AgentInfo {
	return AgentInfo{Name: ag.Name, Tool: ag.Tool, Model: ag.Model, VendorModel: a.s.VendorModel(ag),
		Provider: ag.Provider, Kind: ag.Kind, Warm: ag.Warm, Effort: ag.Effort, Band: ag.Band}
}

// toolVersions caches each tool's reported version for identities. probe
// names the argv that reports it (cligw's VersionProbeArgv: the declared
// binary a worker runs); without one it is `<tool> --version` on PATH.
type toolVersions struct {
	probe func(tool string) []string
	mu    sync.Mutex
	m     map[string]struct {
		v  string
		at time.Time
	}
}

func (t *toolVersions) get(tool string) string {
	t.mu.Lock()
	if e, ok := t.m[tool]; ok && e.v != "unknown" && time.Since(e.at) < time.Minute {
		t.mu.Unlock()
		return e.v
	}
	t.mu.Unlock()
	v := "unknown"
	argv := []string{tool, "--version"}
	if t.probe != nil {
		if p := t.probe(tool); len(p) > 0 {
			argv = p
		}
	}
	if path, err := exec.LookPath(argv[0]); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, path, argv[1:]...).Output()
		cancel()
		if err == nil {
			v = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		}
	}
	t.mu.Lock()
	if t.m == nil {
		t.m = map[string]struct {
			v  string
			at time.Time
		}{}
	}
	t.m[tool] = struct {
		v  string
		at time.Time
	}{v, time.Now()}
	t.mu.Unlock()
	return v
}

// Healthy reports whether a door answers at the configured port.
func Healthy() bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(door.BaseURL() + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	// 401 still proves it is the door: /health needs the token.
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
}

// ErrAlreadyServing: a door already answers on the port.
var ErrAlreadyServing = errors.New("the model door is already serving")

// cliServerOptions maps the door's fleet-agent knobs onto cligw's: the
// --max-in-flight flag wins, then CLIGW_MAX_IN_FLIGHT (parsed by cligw, so
// both doors spell it the same way), then the policy file. Every request
// through the door authorizes as the single "owner" principal — sessions
// and sticky keys do not change it — so max_in_flight_by_principal
// overrides must use the "owner" key.
func cliServerOptions(token string, o DoorOptions) (cligw.ServerOptions, error) {
	maxInFlight := o.MaxInFlight
	if maxInFlight == 0 {
		var err error
		if maxInFlight, err = cligw.MaxInFlightFromEnv(); err != nil {
			return cligw.ServerOptions{}, err
		}
	}
	return cligw.ServerOptions{Token: token, PolicyFile: o.Policy, Prewarm: o.Prewarm, MaxInFlight: maxInFlight}, nil
}

// RunDoor serves the door in the foreground until ctx ends or SIGINT/SIGTERM.
func RunDoor(ctx context.Context, o DoorOptions) error {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	port := o.Port
	if port <= 0 {
		port = door.Port()
	}
	if Healthy() && port == door.Port() {
		fmt.Fprintf(out, "model door already serving on %s\n", door.BaseURL())
		return ErrAlreadyServing
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	token, err := door.Token()
	if err != nil {
		return err
	}
	stateDir, err := door.StateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}

	opts := Options{Token: token, StateDir: stateDir}
	var cli *cligw.Server
	if !o.NoCLI {
		cliOpts, err := cliServerOptions(token, o)
		if err != nil {
			return err
		}
		cli, err = cligw.NewServer(cliOpts)
		if err != nil {
			return err
		}
		defer cli.Close()
		go cli.Autoscaler().Run(ctx)
		versions := &toolVersions{probe: cli.VersionProbeArgv}
		opts.CLI = cliAdapter{s: cli, versions: versions}
		opts.ToolVersion = versions.get
	}
	if !o.NoEngine && EngineArgv != nil {
		engineCtx := DefaultEngineContext
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OLLAMA_CONTEXT_LENGTH"))); err == nil && v > 0 {
			engineCtx = v
		}
		logf, err := os.OpenFile(filepath.Join(stateDir, "engine.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		opts.Engine = &ExecEngine{Argv: EngineArgv(), Env: []string{"OLLAMA_CONTEXT_LENGTH=" + strconv.Itoa(engineCtx)}, Log: logf}
		opts.EngineContext = engineCtx
	}
	mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if sys, err := resources.Collect(mctx, resources.Options{}); err == nil && sys != nil {
		opts.MemoryBytes = sys.Memory.TotalBytes
	}
	cancel()

	b, err := New(ctx, opts)
	if err != nil {
		return err
	}
	defer b.Close(context.Background())

	host := "127.0.0.1"
	switch o.Bind {
	case "", cligw.BindLoopback:
	case cligw.BindLAN:
		host = ""
	default:
		return fmt.Errorf("broker: unknown bind %q (want loopback or lan)", o.Bind)
	}
	tcp, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("broker: listen on port %d: %w", port, err)
	}
	listeners := []net.Listener{tcp}
	if sock, err := door.SocketPath(); err == nil && peerCredentialsSupported {
		_ = os.Remove(sock)
		if ln, err := net.Listen("unix", sock); err == nil {
			if err := os.Chmod(sock, 0o600); err != nil {
				ln.Close()
				tcp.Close()
				return err
			}
			listeners = append(listeners, ln)
			defer os.Remove(sock)
		}
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if cli != nil {
		// cligw's own verbs (`llm pools`) reach it under /cligw on the door.
		_ = cligw.WriteEndpoint(cligw.Endpoint{SchemaVersion: cligw.EndpointSchemaVersion, BaseURL: base + "/cligw",
			Bind: firstNonEmpty(o.Bind, cligw.BindLoopback), Port: port, PID: os.Getpid(), Anthropic: true, StartedAt: time.Now().UTC()})
		defer cligw.RemoveEndpoint()
	}
	fmt.Fprintf(out, "model door serving on %s (pid %d)\n", base, os.Getpid())
	if opts.Engine != nil {
		fmt.Fprintf(out, "local engine: bashy's Ollama, exclusive device, context %d\n", opts.EngineContext)
	}
	if cli != nil {
		fmt.Fprintln(out, "fleet agents: cligw (band/model/agent selectors)")
	}
	fmt.Fprintln(out, "eval \"$(bashy llm env)\" to configure a client")
	return b.Serve(ctx, listeners...)
}

// ---- the `llm` command tree -------------------------------------------------

// NewCmd returns the `llm` command tree: cligw's verbs with `serve` and `env`
// replaced by the door's, plus `up`, `down` and `sticky`.
func NewCmd() *cobra.Command {
	cmd := cligw.NewCmd()
	cmd.Short = "the host's model door: local models and fleet agents behind one endpoint"
	cmd.Long = `llm is the host's one door to its model capacity (port 24556, "AILLM" on a
phone keypad). Every shell, subshell and agent goes through it:

  local models   bashy's own Ollama engine, run as an exclusive device with a
                 priority run queue (interrupt > steering > interactive > batch)
  fleet agents   the CLI seats this host pays for (claude, codex, agy, ...),
                 served by cligw: model L4, L4+, opus5, codex-gpt-5.5, ...

Sticky bindings freeze the exact instance (model digest or CLI version,
launch fingerprint, options) for N requests, so benchmark arms compare
harnesses, not routing luck: see ` + "`llm sticky --help`" + `.

Clients authenticate with the owner token (bearer, or the /k/<token> URL form
for clients that only take a base URL) or the owner-only unix socket.`
	for _, c := range cmd.Commands() {
		if c.Name() == "serve" || c.Name() == "env" {
			cmd.RemoveCommand(c)
		}
	}
	cmd.AddCommand(newServeCmd(), newUpCmd(), newDownCmd(), newEnvCmd(), newStickyCmd())
	return cmd
}

func newServeCmd() *cobra.Command {
	var o DoorOptions
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "serve the model door in the foreground",
		Long: `serve runs the host's model door: it starts bashy's Ollama engine on a private
loopback port and serves local models and fleet agents on port 24556 (loopback
by default; --bind lan must be asked for), plus an owner-only unix socket.

Only one door runs per host: when one already answers, serve says so and exits.

Parallel fleet-agent requests share one per-principal in-flight cap (default
4): past it the door answers 429. Every request authorizes as the single
"owner" principal — sessions (X-Bashy-Session, /s/...) and sticky keys do
not change it — so max_in_flight_by_principal overrides must use the
"owner" key. Raise the cap with --max-in-flight (or CLIGW_MAX_IN_FLIGHT,
or max_in_flight in policy.yaml).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			o.Out = c.OutOrStdout()
			err := RunDoor(c.Context(), o)
			if errors.Is(err, ErrAlreadyServing) {
				return nil
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Bind, "bind", cligw.BindLoopback, "where to listen: loopback|lan")
	f.IntVar(&o.Port, "port", 0, "TCP port (default 24556 or $BASHY_LLM_PORT)")
	f.StringVar(&o.Policy, "policy", "", "cligw routing policy file")
	f.BoolVar(&o.Prewarm, "prewarm", false, "spawn each band leader's spare CLI worker at startup")
	f.IntVar(&o.MaxInFlight, "max-in-flight", 0, "per-principal in-flight cap for fleet agents (default 4; overrides CLIGW_MAX_IN_FLIGHT and max_in_flight in policy.yaml; the principal is always \"owner\")")
	f.BoolVar(&o.NoEngine, "no-engine", false, "serve fleet agents only (no local models)")
	f.BoolVar(&o.NoCLI, "no-cli", false, "serve local models only (no fleet agents)")
	return cmd
}

// EnsureUp starts the door in the background when none answers, and waits
// until it does. Idempotent.
func EnsureUp(ctx context.Context) error {
	if Healthy() {
		return nil
	}
	if SelfArgv == nil {
		return errors.New("broker: this binary cannot start the door (no self command)")
	}
	stateDir, err := door.StateDir()
	if err != nil {
		return err
	}
	_ = os.MkdirAll(stateDir, 0o700)
	logf, err := os.OpenFile(filepath.Join(stateDir, "door.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	argv := SelfArgv()
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdout, c.Stderr, c.Stdin = logf, logf, nil
	c.SysProcAttr = detachSysProcAttr()
	if err := c.Start(); err != nil {
		return fmt.Errorf("broker: start the door: %w", err)
	}
	_ = c.Process.Release()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if Healthy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("broker: the door did not come up within 120s (see %s)", filepath.Join(stateDir, "door.log"))
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "up",
		Short:         "make sure the model door is running (start it in the background if not)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := EnsureUp(c.Context()); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "model door up on %s\n", door.BaseURL())
			return nil
		},
	}
}

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "down",
		Short:         "stop the running model door",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			var h Health
			if err := doorJSON(c.Context(), http.MethodGet, "/health", nil, &h); err != nil {
				return fmt.Errorf("no door answers on %s", door.BaseURL())
			}
			p, err := os.FindProcess(h.PID)
			if err != nil {
				return err
			}
			if err := stopDoorProcess(p); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "stopping the model door (pid %d)\n", h.PID)
			return nil
		},
	}
}

// EnvSchemaVersion is the envelope `llm env --json` emits. The five client
// variables sit under env exactly as the shell form exports them.
const EnvSchemaVersion = "bashy-llm-env-v1"

// envVarOrder is the shell form's line order and its export list.
var envVarOrder = []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "OLLAMA_HOST"}

// bindStickySession finds the session that owns the principal's sticky KEY, so
// the minted URL can name it: the binding is then reachable from any shell,
// whatever session that shell has. With spec.Model set it also creates the
// binding (in the caller's session) when the key does not exist yet. found
// is false when the key is unknown or the door cannot be asked; the caller
// then keeps its own session, which still lets the first request create the
// binding implicitly with an X-Bashy-Sticky header.
func bindStickySession(ctx context.Context, spec StickySpec) (session string, found bool, err error) {
	var view map[string]any
	err = doorJSON(ctx, http.MethodGet, "/v1/sticky/"+spec.Key+principalScopePath, nil, &view)
	var se *doorStatusError
	var un *doorUnreachable
	switch {
	case err == nil:
		return str(view["session"]), true, nil
	case errors.As(err, &un):
		if spec.Model != "" {
			return "", false, err
		}
		return "", false, nil
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		if spec.Model == "" {
			return "", false, nil
		}
	default:
		return "", false, err
	}
	if err := doorJSON(ctx, http.MethodPost, "/v1/sticky", spec, &view); err != nil {
		return "", false, err
	}
	return str(view["session"]), true, nil
}

func newEnvCmd() *cobra.Command {
	var sticky string
	var asJSON bool
	var spec StickySpec
	cmd := &cobra.Command{
		Use:   "env",
		Short: "print the environment a client needs (OpenAI, Anthropic and Ollama clients)",
		Long: `env prints the variables that point clients at the door:

  OPENAI_BASE_URL / OPENAI_API_KEY        OpenAI SDKs, litellm, ycode
  ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY  Anthropic SDKs
  OLLAMA_HOST                             the ollama client (token in the path)

The key is the owner token, not a vendor key. With --sticky KEY the base URLs
carry the binding, so a client that only takes a base URL (mini-swe-agent via
litellm) is bound without code changes. If the sticky already exists, the URLs
name the session that owns it, so this works from any shell or agent tool call
of the same principal: create it in one, bind to it in the next. With --model
M the sticky is created first when it does not exist (one step: freeze + URL).
Otherwise the caller's $BASHY_MODEL_SESSION, if set, is carried in the URLs.

--json emits the bashy-llm-env-v1 envelope (base_url, session, sticky, env)
instead of shell assignments. Both forms carry the owner token: never commit
either, record only the schema name as evidence.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			token, err := door.Token()
			if err != nil {
				return err
			}
			session := strings.TrimSpace(os.Getenv(SessionEnv))
			if sticky != "" {
				spec.Key = sticky
				if err := spec.validate(); err != nil {
					return err
				}
				owner, found, err := bindStickySession(c.Context(), spec)
				if err != nil {
					return err
				}
				if found {
					session = owner
				}
			}
			prefix := ""
			if session != "" {
				prefix += "/s/" + session
			}
			if sticky != "" {
				prefix += "/sticky/" + sticky
			}
			base := door.BaseURL()
			out := c.OutOrStdout()
			env := map[string]string{
				"OPENAI_BASE_URL":    base + prefix + "/v1",
				"OPENAI_API_KEY":     token,
				"ANTHROPIC_BASE_URL": base + prefix + "/anthropic",
				"ANTHROPIC_API_KEY":  token,
				"OLLAMA_HOST":        base + "/k/" + token + prefix,
			}
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					SchemaVersion string            `json:"schema_version"`
					BaseURL       string            `json:"base_url"`
					Session       string            `json:"session"`
					Sticky        string            `json:"sticky"`
					Env           map[string]string `json:"env"`
				}{EnvSchemaVersion, base, session, sticky, env})
			}
			for _, k := range envVarOrder {
				fmt.Fprintf(out, "%s=%s\n", k, env[k])
			}
			fmt.Fprintln(out, "export "+strings.Join(envVarOrder, " "))
			fmt.Fprintln(out, "# OpenAI routes: /v1/chat/completions /v1/responses /v1/models")
			return nil
		},
	}
	cmd.Flags().StringVar(&sticky, "sticky", "", "bind the printed base URLs to this sticky key (any session's)")
	cmd.Flags().StringVar(&spec.Model, "model", "", "with --sticky: create the sticky from this model/band/agent if KEY does not exist")
	cmd.Flags().IntVar(&spec.Uses, "uses", 0, "with --model: number of requests (0 = unbounded)")
	cmd.Flags().StringVar(&spec.TTL, "ttl", "", "with --model: idle expiry (default 30m; 0 = never)")
	cmd.Flags().StringVar(&spec.Bind, "bind", "", "with --model: identity|worker")
	cmd.Flags().StringVar(&spec.Reset, "reset", "", "with --model: each|none")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the "+EnvSchemaVersion+" envelope instead of shell assignments")
	return cmd
}

// doorStatusError is a non-2xx answer from the door.
type doorStatusError struct {
	Status int
	Msg    string
}

func (e *doorStatusError) Error() string { return e.Msg }

// doorUnreachable marks a failure to reach the door at all (not an answer).
type doorUnreachable struct{ err error }

func (e *doorUnreachable) Error() string { return e.err.Error() }
func (e *doorUnreachable) Unwrap() error { return e.err }

// principalScopePath asks the door for the owner's view: every session of the
// principal, not just the ones the caller's session can see.
const principalScopePath = "?scope=principal"

// doorJSON calls the door with the owner token and the caller's session.
func doorJSON(ctx context.Context, method, path string, body, out any) error {
	token, err := door.Token()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, door.BaseURL()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if s := strings.TrimSpace(os.Getenv(SessionEnv)); s != "" {
		req.Header.Set(SessionHeader, s)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return &doorUnreachable{fmt.Errorf("reach the model door at %s: %w (start it: bashy llm up)", door.BaseURL(), err)}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return &doorStatusError{Status: resp.StatusCode, Msg: e.Error.Message}
		}
		return &doorStatusError{Status: resp.StatusCode, Msg: fmt.Sprintf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func newStickyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sticky",
		Short: "sticky bindings: the exact same model instance across requests",
		Long: `A sticky binding resolves a model ONCE and freezes its identity — local model
and digest, or fleet agent, CLI version, launch fingerprint and the agent's
declared effort (only when one is declared: undeclared adds nothing, so older
digests are unchanged), plus options — then serves every request under its key
with that identity or refuses (never reroutes; a CLI upgrade or an effort
change after creation is refused with 409). Use one per benchmark comparison,
shared across arms by digest:

  bashy llm sticky create g03-opus5 --model claude-opus5
  bashy llm sticky create arm-genie --identity <digest from above>
  eval "$(bashy llm env --sticky arm-genie)"   # base URLs carry the binding

Attributes: --uses N (0 = unbounded; request N+1 is refused), --bind
identity|worker, --reset each|none (none = multi-turn on one worker, needs
bind=worker), --ttl (idle expiry, default 30m, 0 = never), --export (visible
to child processes' sessions).

Across shells: every agent tool call is a new shell, hence a new door session,
and a sticky lives in the session that created it. The owner token manages all
of the principal's stickies from any session: ls lists them with their owning
session, show and rm reach any of them, and

  bashy llm env --sticky KEY            # URLs name the owning session
  bashy llm env --sticky KEY --model M  # create (if new) + URLs, one step

mint a /k/<token> URL bound to the sticky whatever shell runs them. Creating a
key another session owns is refused with the owner's session id. Requests may also bind on the fly with the
header X-Bashy-Sticky: KEY; uses=N; ... or the body field "bashy": {"sticky": {...}}.`,
	}
	cmd.AddCommand(newStickyCreateCmd(), newStickyShowCmd(), newStickyListCmd(), newStickyRmCmd())
	return cmd
}

func newStickyCreateCmd() *cobra.Command {
	var spec StickySpec
	var numCtx, seed int
	var temperature float64
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "create KEY",
		Short:         "resolve a model once and freeze it under KEY",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			spec.Key = args[0]
			f := c.Flags()
			opts := map[string]any{}
			if f.Changed("num-ctx") {
				opts["num_ctx"] = numCtx
			}
			if f.Changed("temperature") {
				opts["temperature"] = temperature
			}
			if f.Changed("seed") {
				opts["seed"] = seed
			}
			if len(opts) > 0 {
				spec.Options = opts
			}
			var view map[string]any
			if err := doorJSON(c.Context(), http.MethodPost, "/v1/sticky", spec, &view); err != nil {
				return err
			}
			return printSticky(c.OutOrStdout(), view, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&spec.Model, "model", "", "model, band (L4), registry model or agent to freeze")
	f.StringVar(&spec.Identity, "identity", "", "require (or copy) this identity digest")
	f.StringVar(&spec.Filter, "filter", "", "cligw filter for band/model resolution")
	f.IntVar(&spec.Uses, "uses", 0, "number of requests (0 = unbounded)")
	f.StringVar(&spec.Bind, "bind", BindIdentity, "identity|worker")
	f.StringVar(&spec.Reset, "reset", ResetEach, "each|none")
	f.StringVar(&spec.TTL, "ttl", "", "idle expiry (default 30m; 0 = never)")
	f.BoolVar(&spec.Export, "export", false, "visible to child processes' sessions")
	f.IntVar(&numCtx, "num-ctx", 0, "context length (local models)")
	f.Float64Var(&temperature, "temperature", 0, "sampling temperature")
	f.IntVar(&seed, "seed", 0, "sampling seed (local models)")
	f.BoolVar(&asJSON, "json", false, "emit the bashy-sticky-v1 envelope")
	return cmd
}

func newStickyShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "show KEY", Short: "show a binding's identity and use count", Args: cobra.ExactArgs(1),
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			var view map[string]any
			if err := doorJSON(c.Context(), http.MethodGet, "/v1/sticky/"+args[0]+principalScopePath, nil, &view); err != nil {
				return err
			}
			return printSticky(c.OutOrStdout(), view, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-sticky-v1 envelope")
	return cmd
}

func newStickyListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "ls", Short: "list this principal's bindings across all sessions (marks the owning session)", Args: cobra.NoArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			var list struct {
				Bindings []map[string]any `json:"bindings"`
			}
			if err := doorJSON(c.Context(), http.MethodGet, "/v1/sticky"+principalScopePath, nil, &list); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(list)
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tIDENTITY\tBACKEND\tMODEL\tUSED\tREMAINING\tSESSION")
			for _, v := range list.Bindings {
				id, _ := v["identity"].(map[string]any)
				model := firstNonEmpty(str(id["agent"]), str(id["model"]))
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%v\t%s\n", str(v["key"]), str(v["short"]), str(id["backend"]), model, v["used"], remainingText(v["remaining"]), sessionText(v))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newStickyRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm KEY", Short: "delete a binding (from any session of this principal)", Args: cobra.ExactArgs(1),
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := doorJSON(c.Context(), http.MethodDelete, "/v1/sticky/"+args[0]+principalScopePath, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}

func printSticky(w io.Writer, view map[string]any, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	id, _ := view["identity"].(map[string]any)
	fmt.Fprintf(w, "key       %s\n", str(view["key"]))
	fmt.Fprintf(w, "session   %s\n", sessionText(view))
	fmt.Fprintf(w, "identity  %s\n", str(view["digest"]))
	fmt.Fprintf(w, "backend   %s\n", str(id["backend"]))
	if a := str(id["agent"]); a != "" {
		fmt.Fprintf(w, "agent     %s (%s %s, %s)\n", a, str(id["tool"]), str(id["tool_version"]), str(id["vendor_model"]))
	} else {
		fmt.Fprintf(w, "model     %s (%s)\n", str(id["model"]), str(id["model_digest"]))
	}
	if e := str(id["effort"]); e != "" {
		fmt.Fprintf(w, "effort    %s\n", e)
	}
	if o, ok := id["options"].(map[string]any); ok && len(o) > 0 {
		data, _ := json.Marshal(o)
		fmt.Fprintf(w, "options   %s\n", data)
	}
	fmt.Fprintf(w, "used      %v (remaining %s)\n", view["used"], remainingText(view["remaining"]))
	return nil
}

// sessionText marks a listing row's owning session; "(here)" = the caller's
// session can serve it directly, anything else is reached with
// `llm env --sticky KEY`.
func sessionText(v map[string]any) string {
	owner := str(v["session"])
	if owner == "" {
		owner = "(principal)"
	}
	if vis, _ := v["visible"].(bool); vis && str(v["session"]) != "" {
		return owner + " (here)"
	}
	return owner
}

func remainingText(v any) string {
	if f, ok := v.(float64); ok && f < 0 {
		return "unbounded"
	}
	return fmt.Sprint(v)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
