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

// ServiceUnit names the fleet service unit backing the model door
// (pkg/fleet/baseline/services/llm.yaml).
const ServiceUnit = "llm"

// SetServiceEnabled persists ServiceUnit's desired state — bashy wires it to
// pkg/fleet's Catalog.SetServiceEnabled on its own catalog, the same write
// `bashy service enable/disable llm` makes. `llm down --service` composes it
// with its own stop; `llm up --service` leaves starting the door to
// supervisord (see EnsureSupervisordRunning) rather than starting it itself.
// Nil means this binary has no service layer, so --service is refused
// outright. A package variable, not a direct pkg/fleet import, so a test
// fakes the service layer without writing to a real catalog.
var SetServiceEnabled func(ctx context.Context, unit string, enabled bool) error

// EnsureSupervisordRunning starts bashy supervisord under the current
// BASHY_HOME when nothing answers its control socket, and waits until
// something does. `llm up --service` calls it instead of starting the door
// itself: supervisord is the only process that restarts a crashed unit or
// brings it back up at boot, and a door started directly here would be a
// second, unsupervised process holding the port — exactly what a
// supervisord started afterwards cannot tell apart from a foreign listener.
// Nil means this binary has no service layer, same as SetServiceEnabled. A
// package variable for the same reason: bashy wires it to pkg/fleet's
// EnsureSupervisordRunning, and a test fakes it without starting a real
// monitor.
var EnsureSupervisordRunning func(ctx context.Context) error

// errNoServiceLayer is returned by `llm up/down --service` when
// SetServiceEnabled or EnsureSupervisordRunning is nil.
var errNoServiceLayer = errors.New("broker: this binary has no service layer (--service unavailable)")

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
	Capture     bool
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

	peersFile, err := door.PeersPath()
	if err != nil {
		return err
	}
	opts := Options{Token: token, StateDir: stateDir, PeersFile: peersFile, Capture: o.Capture || os.Getenv("BASHY_LLM_CAPTURE") == "1"}
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
		ln, err := listenOwnerSocket(sock)
		if err != nil {
			tcp.Close()
			return err
		}
		listeners = append(listeners, ln)
		defer os.Remove(sock)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if cli != nil {
		// cligw's own verbs (`llm pools`) reach it under /cligw on the door.
		_ = cligw.WriteEndpoint(cligw.Endpoint{SchemaVersion: cligw.EndpointSchemaVersion, BaseURL: base + "/cligw",
			Bind: firstNonEmpty(o.Bind, cligw.BindLoopback), Port: port, PID: os.Getpid(), Anthropic: true, StartedAt: time.Now().UTC()})
		defer cligw.RemoveEndpoint()
	}
	fmt.Fprintf(out, "model door serving on %s (pid %d)\n", base, os.Getpid())
	if b.local {
		fmt.Fprintf(out, "local engine: bashy's Ollama, exclusive device, context %d\n", opts.EngineContext)
	} else if b.noEngineByBuild {
		fmt.Fprintln(out, "local engine: unavailable in this build (lean binary without the engines tag); serving fleet agents only")
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
for clients that only take a base URL) or the owner-only unix socket. A sphere
peer gets its own credential (` + "`llm peer add NAME`" + `), admitted only while
its share policy is allow.`
	for _, c := range cmd.Commands() {
		if c.Name() == "serve" || c.Name() == "env" {
			cmd.RemoveCommand(c)
		}
	}
	cmd.AddCommand(newServeCmd(), newUpCmd(), newDownCmd(), newEnvCmd(), newStatusCmd(), newStickyCmd(), newPeerCmd(), newCaptureCmd())
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
	f.BoolVar(&o.Capture, "capture", false, "record redacted inference turns by session")
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
	var logStart int64
	if fi, err := logf.Stat(); err == nil {
		logStart = fi.Size()
	}
	argv := SelfArgv()
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdout, c.Stderr, c.Stdin = logf, logf, nil
	c.SysProcAttr = detachSysProcAttr()
	if err := c.Start(); err != nil {
		return fmt.Errorf("broker: start the door: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if Healthy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case werr := <-exited:
			if Healthy() {
				return nil
			}
			return fmt.Errorf("broker: the door exited before it came up (%v): %s", exitReason(werr), lastLogLines(filepath.Join(stateDir, "door.log"), logStart))
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("broker: the door did not come up within 120s (see %s)", filepath.Join(stateDir, "door.log"))
}

// waitDoorHealthy polls the door's health endpoint until it answers, ctx
// ends, or 120s pass. `llm up --service` calls it after supervisord starts
// the door: the process belongs to supervisord, not a c.Wait() this
// package started, so health polling is all it has to go on (unlike
// EnsureUp, which starts the door itself and also watches for an early
// exit).
func waitDoorHealthy(ctx context.Context) error {
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
	msg := "broker: the door did not come up within 120s"
	if stateDir, err := door.StateDir(); err == nil {
		msg += " (see " + filepath.Join(stateDir, "door.log") + ")"
	}
	return errors.New(msg)
}

func newUpCmd() *cobra.Command {
	var asService bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "make sure the model door is running (start it in the background if not)",
		Long: `up starts the door if none answers. With --service it persists the llm
unit's desired state as enabled (the same write "bashy service enable llm"
makes), makes sure bashy supervisord itself is running (starting it under
this BASHY_HOME if nothing answers its control socket), and waits for the
door supervisord starts — rather than starting the door itself, which
nothing would then restart or bring back up at login/boot, and which a
supervisord started afterwards would see as a foreign listener on the
unit's port.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if !asService {
				if err := EnsureUp(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "model door up on %s\n", door.BaseURL())
				return nil
			}
			if SetServiceEnabled == nil || EnsureSupervisordRunning == nil {
				return errNoServiceLayer
			}
			if err := SetServiceEnabled(c.Context(), ServiceUnit, true); err != nil {
				return err
			}
			if err := EnsureSupervisordRunning(c.Context()); err != nil {
				return err
			}
			if err := waitDoorHealthy(c.Context()); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "llm unit enabled; model door up on %s\n", door.BaseURL())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asService, "service", false, "enable the llm unit (bashy supervisord autostarts and restarts it) in addition to starting it now")
	return cmd
}

// doorStopper stops the door process; stopDoorProcess by default. A package
// variable so a test can observe its call order against SetServiceEnabled
// without depending on the exact signal/process semantics of a real stop.
var doorStopper = stopDoorProcess

func newDownCmd() *cobra.Command {
	var asService bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "stop the running model door",
		Long: `down stops the door process answering on its port. With --service it also
persists the llm unit's desired state as disabled (the same write "bashy
service disable llm" makes), so bashy supervisord stops bringing it back up
at login/boot and stops restarting it on crash. A door that is not
currently running is not an error under --service: the unit is still
disabled for next time.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if asService {
				if SetServiceEnabled == nil {
					return errNoServiceLayer
				}
				// Disable before stopping: once the unit is disabled,
				// supervisord does not restart it when the process exits. In
				// the other order, the process's exit can race supervisord's
				// on-failure policy, which restarts it once before it reads
				// the disable.
				if err := SetServiceEnabled(c.Context(), ServiceUnit, false); err != nil {
					return err
				}
			}
			var h Health
			running := doorJSON(c.Context(), http.MethodGet, "/health", nil, &h) == nil
			if running {
				p, err := os.FindProcess(h.PID)
				if err != nil {
					return err
				}
				if err := doorStopper(p); err != nil {
					return err
				}
			} else if !asService {
				return fmt.Errorf("no door answers on %s", door.BaseURL())
			}
			out := c.OutOrStdout()
			switch {
			case running && asService:
				fmt.Fprintf(out, "stopping the model door (pid %d) and disabling the llm unit\n", h.PID)
			case running:
				fmt.Fprintf(out, "stopping the model door (pid %d)\n", h.PID)
			default:
				fmt.Fprintln(out, "llm unit disabled (no door was running)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asService, "service", false, "disable the llm unit (bashy supervisord no longer starts or restarts it) in addition to stopping it now")
	return cmd
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
	var client string
	var via string
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

--client generic prints the shell environment (and is the default). OpenClaw
prints a complete JSON provider block selecting bashy/L4. Codync prints the
env block for its service unit; codync-host passes that environment to the ACP
agents it starts.

--via mesh is configuration-only. First expose the door on its host and bind
the remote service to this host's ordinary door address, then print the same
client block with that dialed loopback address:

  door$ outpost mesh service add llm 127.0.0.1:24556
  door$ bashy llm env --client openclaw --via mesh >openclaw-bashy.json
  client$ outpost mesh dial --local 127.0.0.1:24556 llm

Generate the block on the door host so it contains that door's owner token,
then transfer it securely to the client. Set BASHY_LLM_PORT while printing
when the mesh dial uses another local port.

--json emits the bashy-llm-env-v1 envelope (base_url, session, sticky, env)
instead of shell assignments. Both forms carry the owner token: never commit
either, record only the schema name as evidence.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			client = strings.ToLower(strings.TrimSpace(client))
			switch client {
			case "", "generic", "openclaw", "codync":
			case "openai":
				client = "generic"
			default:
				return fmt.Errorf("llm env: unknown client %q (want generic, openclaw, or codync)", client)
			}
			via = strings.ToLower(strings.TrimSpace(via))
			switch via {
			case "", "local":
				via = "local"
			case "mesh":
			default:
				return fmt.Errorf("llm env: unknown route %q (want local or mesh)", via)
			}
			if asJSON && client != "" && client != "generic" {
				return errors.New("llm env: --json cannot be combined with --client openclaw or codync")
			}
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
			if session != "" {
				if episode := firstNonEmpty(os.Getenv("BASHY_EPISODE"), os.Getenv("WEAVE_EPISODE")); episode != "" {
					if err := writeCaptureSession(session, episode); err != nil {
						return err
					}
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
			return writeClientEnv(out, client, via, env)
		},
	}
	cmd.Flags().StringVar(&sticky, "sticky", "", "bind the printed base URLs to this sticky key (any session's)")
	cmd.Flags().StringVar(&spec.Model, "model", "", "with --sticky: create the sticky from this model/band/agent if KEY does not exist")
	cmd.Flags().IntVar(&spec.Uses, "uses", 0, "with --model: number of requests (0 = unbounded)")
	cmd.Flags().StringVar(&spec.TTL, "ttl", "", "with --model: idle expiry (default 30m; 0 = never)")
	cmd.Flags().StringVar(&spec.Bind, "bind", "", "with --model: identity|worker")
	cmd.Flags().StringVar(&spec.Reset, "reset", "", "with --model: each|none")
	cmd.Flags().StringVar(&client, "client", "generic", "ready configuration for generic|openclaw|codync")
	cmd.Flags().StringVar(&via, "via", "local", "reach the door through local|mesh (mesh uses the dialed local BASHY_LLM_PORT)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the "+EnvSchemaVersion+" envelope instead of shell assignments")
	return cmd
}

func writeClientEnv(out io.Writer, client, via string, env map[string]string) error {
	if client == "" {
		client = "generic"
	}
	switch client {
	case "generic":
		if via == "mesh" {
			fmt.Fprintf(out, "# mesh dial: %s\n", strings.TrimSuffix(env["OPENAI_BASE_URL"], "/v1"))
		}
		for _, k := range envVarOrder {
			fmt.Fprintf(out, "%s=%s\n", k, env[k])
		}
		fmt.Fprintln(out, "export "+strings.Join(envVarOrder, " "))
		fmt.Fprintln(out, "# OpenAI routes: /v1/chat/completions /v1/responses /v1/models")
		return nil
	case "openclaw":
		block := map[string]any{
			"models": map[string]any{
				"mode": "merge",
				"providers": map[string]any{
					"bashy": map[string]any{
						"baseUrl": env["OPENAI_BASE_URL"],
						"apiKey":  env["OPENAI_API_KEY"],
						"api":     "openai-completions",
						"models": []map[string]any{{
							"id": "L4", "name": "Bashy fleet L4", "reasoning": true,
							"input": []string{"text"}, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
							"contextWindow": 200000, "maxTokens": 32000,
						}},
					},
				},
			},
			"agents": map[string]any{"defaults": map[string]any{"model": map[string]string{"primary": "bashy/L4"}}},
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(block)
	case "codync":
		fmt.Fprintln(out, "# codync-host service env; inherited by the ACP agents it starts")
		if via == "mesh" {
			fmt.Fprintf(out, "# mesh dial: %s\n", strings.TrimSuffix(env["OPENAI_BASE_URL"], "/v1"))
		}
		fmt.Fprintln(out, "env:")
		for _, k := range envVarOrder[:4] {
			fmt.Fprintf(out, "  %s: %s\n", k, strconv.Quote(env[k]))
		}
		return nil
	default:
		return fmt.Errorf("llm env: unsupported client %q", client)
	}
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

Multi-turn (bind=worker reset=none) holds ONE warm CLI per binding and
forwards only the new turn; it needs a tool that reads further turns from
a streaming stdin protocol (launch warm: stdin-stream-json). Any other tool
is one-shot and the turn is refused with 501 naming the tool, the reason and
the supported tools:

  tool      multi-turn
  claude    yes
  agy       yes
  codex     no — 501: one-shot CLI (warm: stdin); needs a streaming stdin protocol
  muse      no — 501: one-shot CLI (warm: cold); needs a streaming stdin protocol
  opencode  no — 501: one-shot CLI (warm: cold); needs a streaming stdin protocol
  genie     no — 501: one-shot CLI (warm: cold); needs a streaming stdin protocol

bind=worker with reset=each stays one-shot on every tool.

Across shells: every agent tool call is a new shell, hence a new door session,
and a sticky lives in the session that created it. The owner token manages all
of the principal's stickies from any session: ls lists them with their owning
session, show and rm reach any of them, and

  bashy llm env --sticky KEY            # URLs name the owning session
  bashy llm env --sticky KEY --model M  # create (if new) + URLs, one step

mint a /k/<token> URL bound to the sticky whatever shell runs them. Creating a
key another session owns is refused with the owner's session id. Requests may also bind on the fly with the
header X-Bashy-Sticky: KEY; uses=N; ... or the body field "bashy": {"sticky": {...}}.

Principal scope: a service key that every shell and every nested session must
reach (genie's per-model keys genie-<model>, created once, ttl 0) belongs to no
session. Create it with --scope principal; a session-owned key of the same name
is refused until it is removed.

After a CLI upgrade: a frozen key is refused with 409 ("<tool> is now X (was
Y)") — never served by the new version silently. check reports every such key
before a run hits the refusal, and repin re-resolves the key's own spec in
place, keeping its scope (principal stays principal) and refusing to move the
seat (a different agent, vendor model or provider is a model switch: rm and
create it on purpose):

  bashy llm sticky check                      # exit 1 lists the stale keys
  bashy llm sticky repin genie-muse-spark1.3  # same key, same scope, new CLI version
  bashy llm sticky create genie-X --model X --ttl 0 --scope principal   # recreate an absent one`,
	}
	cmd.AddCommand(newStickyCreateCmd(), newStickyShowCmd(), newStickyListCmd(), newStickyRmCmd(), newStickyRepinCmd(), newStickyCheckCmd())
	return cmd
}

func newStickyCreateCmd() *cobra.Command {
	var spec StickySpec
	var numCtx, seed int
	var temperature float64
	var asJSON bool
	var scope string
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
			path := "/v1/sticky"
			switch scope {
			case "session":
			case "principal":
				path += principalScopePath
			default:
				return fmt.Errorf("--scope %q: want session or principal", scope)
			}
			var view map[string]any
			if err := doorJSON(c.Context(), http.MethodPost, path, spec, &view); err != nil {
				return err
			}
			return printSticky(c.OutOrStdout(), view, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "scope", "session", "session (this shell's) or principal (no session: reachable from every shell and nested session)")
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
			fmt.Fprintln(tw, "KEY\tIDENTITY\tBACKEND\tMODEL\tUSED\tREMAINING\tSESSION\tSTATUS")
			for _, v := range list.Bindings {
				id, _ := v["identity"].(map[string]any)
				model := firstNonEmpty(str(id["agent"]), str(id["model"]))
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%v\t%s\t%s\n", str(v["key"]), str(v["short"]), str(id["backend"]), model, v["used"], remainingText(v["remaining"]), sessionText(v), statusText(v))
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

func newStickyRepinCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "repin KEY",
		Short: "re-resolve a binding's own spec in place after a CLI upgrade (same key, same scope, same seat)",
		Long: `repin re-resolves KEY's own spec (model, filter, options, bind, reset, ttl,
export) and freezes the result under the same key in the same scope: a
principal-owned key stays principal-owned, a session-owned key stays with its
session. It is the supported recovery for a key refused with 409 after the
CLI behind it upgraded. The seat never moves: when the spec now resolves to a
different agent, vendor model, provider, backend or location the re-pin is
refused and nothing changes — rm and create to switch models on purpose. The
use count and any held conversation start over; the old identity is reported
beside the new one. Re-pinning an up-to-date key changes nothing.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			var view map[string]any
			if err := doorJSON(c.Context(), http.MethodPut, "/v1/sticky/"+args[0]+principalScopePath, nil, &view); err != nil {
				return err
			}
			if asJSON {
				return printSticky(c.OutOrStdout(), view, true)
			}
			w := c.OutOrStdout()
			if re, _ := view["repinned"].(bool); !re {
				fmt.Fprintf(w, "%s is already served exactly as identity %s; nothing to re-pin\n", str(view["key"]), str(view["short"]))
				return nil
			}
			prev, _ := view["previous_identity"].(map[string]any)
			id, _ := view["identity"].(map[string]any)
			fmt.Fprintf(w, "re-pinned %s: identity %s -> %s\n", str(view["key"]), str(view["previous_short"]), str(view["short"]))
			for _, f := range []struct{ label, key string }{{"tool_version", "tool_version"}, {"launch", "launch"}, {"effort", "effort"}, {"model_digest", "model_digest"}} {
				if was, now := str(prev[f.key]), str(id[f.key]); was != now {
					fmt.Fprintf(w, "  %-13s %q -> %q\n", f.label, was, now)
				}
			}
			return printSticky(w, view, false)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-sticky-v1 envelope (with previous_identity)")
	return cmd
}

func newStickyCheckCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "check [KEY...]",
		Short: "verify each binding can still be served exactly (the CLI behind it did not self-update)",
		Long: `check asks the door, for every binding of this principal (or the KEYs given),
whether a request would be served exactly today or refused: a CLI that
self-updated under a frozen key, a local model re-pulled with a new digest, an
agent whose declared effort changed. Stale keys are listed with the refusal
text and the exit status is 1, so a run can gate on it before its first
request. Recover a stale key with repin.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			var list struct {
				Bindings []map[string]any `json:"bindings"`
			}
			if err := doorJSON(c.Context(), http.MethodGet, "/v1/sticky"+principalScopePath, nil, &list); err != nil {
				return err
			}
			want := map[string]bool{}
			for _, k := range args {
				want[k] = true
			}
			var rows []map[string]any
			var stale []string
			seen := map[string]bool{}
			for _, v := range list.Bindings {
				key := str(v["key"])
				if len(want) > 0 && !want[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, v)
				if ok, _ := v["serviceable"].(bool); !ok {
					stale = append(stale, key)
				}
			}
			var missing []string
			for _, k := range args {
				if !seen[k] {
					missing = append(missing, k)
				}
			}
			// The envelope and the exit status are one contract: a run that
			// gates on `check --json` reads the JSON AND the status, so the
			// report is written first and the stale/missing verdict decides
			// the exit afterwards, exactly as the table form does.
			if asJSON {
				if err := json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{
					"schema_version": "bashy-sticky-check-v1", "bindings": rows, "stale": stale, "missing": missing, "ok": len(stale) == 0 && len(missing) == 0,
				}); err != nil {
					return err
				}
			} else {
				tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "KEY\tIDENTITY\tSESSION\tSTATUS")
				for _, v := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", str(v["key"]), str(v["short"]), sessionText(v), statusText(v))
				}
				for _, k := range missing {
					fmt.Fprintf(tw, "%s\t-\t-\tmissing: no binding of this principal; create it (bashy llm sticky create %s --model M)\n", k, k)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if len(stale) > 0 || len(missing) > 0 {
				return fmt.Errorf("%d stale, %d missing: re-pin stale keys with `bashy llm sticky repin KEY`", len(stale), len(missing))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-sticky-check-v1 envelope (the exit status still reports stale/missing)")
	return cmd
}

// statusText is a listing row's live status: ok, or the refusal a request
// would get today.
func statusText(v map[string]any) string {
	if ok, _ := v["serviceable"].(bool); ok {
		return "ok"
	}
	if s := str(v["stale"]); s != "" {
		return "stale: " + strings.TrimPrefix(s, "sticky: ")
	}
	return "unknown"
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
	if _, checked := view["serviceable"]; checked {
		fmt.Fprintf(w, "status    %s\n", statusText(view))
	}
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

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "report whether the model door is up, and what it serves (exit 1 when down)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := door.Probe(c.Context())
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				if err := enc.Encode(st); err != nil {
					return err
				}
			} else if st.Up {
				fmt.Fprintf(w, "model door up on %s (pid %d)\n", st.Listen, st.PID)
				if st.Version != "" {
					fmt.Fprintf(w, "version: %s\n", st.Version)
				}
				fmt.Fprintf(w, "socket: %s\n", st.Socket)
				for _, b := range st.Backends {
					fmt.Fprintf(w, "backend %s: present=%t healthy=%t\n", b.Name, b.Present, b.Healthy)
				}
				fmt.Fprintf(w, "local models: %d, agent entries: %d, max in flight: %d, queue depth: %d\n", st.LocalModels, st.AgentEntries, st.MaxInFlight, st.QueueDepth)
				if st.Error != "" {
					fmt.Fprintf(w, "warning: %s\n", st.Error)
				}
			} else {
				fmt.Fprintf(w, "model door down: %s\n", st.Error)
			}
			if !st.Up {
				return ErrDoorDown
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-llm-status-v1 envelope")
	return cmd
}

// ErrDoorDown is `llm status`' failure when no door answers (exit 1); the
// report itself has already been printed.
var ErrDoorDown = errors.New("the model door is down")

func exitReason(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// lastLogLines returns what the child wrote to the log after offset, trimmed
// to its last few lines, so a clear startup error is shown instead of a timeout.
func lastLogLines(path string, offset int64) string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) <= offset {
		return "no output (see " + path + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(data[offset:])), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "; ")
}
