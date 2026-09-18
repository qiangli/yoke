package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/fleet"
)

// weaveDefaultFleet is the canonical agent CLI roster used when --fleet is
// not given. It matches the autopilot/orchestrator fleet documented in the
// weave skill.
var weaveDefaultFleet = []string{"claude", "codex", "opencode", "agy"}

// fleetProbeTTL bounds how long a cached --probe capability result is trusted
// before re-probing. Existence (PATH lookup) is cheap and never cached.
const fleetProbeTTL = time.Hour

func newWeaveFleetCmd() *cobra.Command {
	var flags weaveOutputFlags
	var fleetCSV string
	var probe bool
	var auth bool
	var agents bool
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Show each fleet member's availability (installed? on PATH? cooling down? signed in? model usable?)",
		Long: `fleet reports, for each roster entry, whether it is assignable right
now — and why not if not. This is the surface an orchestrator queries BEFORE
assigning work, so it can skip what it cannot launch and fail over to what it
can:

  installed   the binary resolves on PATH (exec.LookPath) — a tool that is
              not installed/not on PATH is reported NOT FOUND, so the
              orchestrator never wastes a launch on it (the failure mode that
              otherwise surfaces only as a 0-second 'weave start' failure).
  cooldown    a tool whose last real invocation hit a provider quota or
              rate limit (a throttled 'weave start', or a pair review whose
              reviewer died on its usage cap) is recorded on cooldown; fleet
              reports it QUOTA-EXHAUSTED or COOLING-DOWN with the reset
              date+time — never 'available' — until the reset passes.
	probe       with --probe, run a real minimal headless turn once and require
	              ` + fleet.SmokeToken + ` in stdout. Exit status alone is never trusted. The
	              verdict is cached (` + fleetProbeTTL.String() + ` TTL) under the queue dir.

  model       a roster entry naming an AGENT (a nickname, an alias, or a bare
              tool:model) additionally requires its model to be usable — a
              metered model with no vault key is not assignable no matter how
              healthy its tool is. Bare tool entries have no model, so this
              check does not apply to them.

Without --probe, a row is only reported as installed: PATH evidence says
nothing about whether the tool can do work. A probed row is usable only when
its smoke turn produced the required stdout token.

Availability is mostly a TOOL property — PATH, throttle, sign-in all belong to
the binary, and two agents sharing a tool share them. What an agent adds is the
model: the half that decides whether the launch is even meaningful.

The roster defaults to ` + fmt.Sprintf("%v", weaveDefaultFleet) + ` (tools);
entries may name agents instead (--fleet 007,codex-gpt-5.5), and --agents
expands the roster to every agent in the registry.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveFleet(cmd, fleetCSV, probe, auth, agents, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&fleetCSV, "fleet", "", "Comma-separated roster of agents (007, claude:opus) or tools (default claude,codex,opencode,agy)")
	cmd.Flags().BoolVar(&agents, "agents", false, "Roster every agent in the registry (`bashy agent list`) instead of the default tool roster")
	cmd.Flags().BoolVar(&probe, "probe", false, "Run and cache an output-gated smoke turn (may use provider capacity)")
	cmd.Flags().BoolVar(&auth, "auth", false, "Also probe AUTH-readiness (headless launch on a trivial prompt; catches a tool that is installed but not signed in, before it stalls a real run)")
	cmd.AddCommand(newWeaveFleetInterviewCmd())
	cmd.AddCommand(newWeaveFleetReviewCmd())
	return cmd
}

// newWeaveFleetReviewCmd implements
// `weave fleet review <tool> --role <role> --rating <0-5> --verdict "…" [--note …]`:
// records a qualitative suitability review for a role into the system-wide
// profile store, so the conductor's routing decisions are documented + tracked.
func newWeaveFleetReviewCmd() *cobra.Command {
	var flags weaveOutputFlags
	var role, verdict string
	var rating int
	var notes []string
	cmd := &cobra.Command{
		Use:   "review <tool>",
		Short: "Record a role-suitability review (rating + verdict) into the profile store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			dir, err := weaveToolsDir()
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet review", weavecli.ExitGenericFail, err))
			}
			if role == "" {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet review", weavecli.ExitGenericFail,
					fmt.Errorf("--role is required")))
			}
			if err := setRoleReview(dir, args[0], role, rating, verdict, notes, time.Now()); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet review", weavecli.ExitGenericFail, err))
			}
			if mode != weavecli.OutputJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "%s as %s: %d/5 — %s\n", args[0], role, rating, verdict)
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave fleet review",
				map[string]any{"tool": args[0], "role": role, "rating": rating, "verdict": verdict}))
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&role, "role", "", "Role being reviewed (conductor|coder|qa|doc|…)")
	cmd.Flags().IntVar(&rating, "rating", 0, "Suitability rating 0–5")
	cmd.Flags().StringVar(&verdict, "verdict", "", "One-line verdict")
	cmd.Flags().StringArrayVar(&notes, "note", nil, "Rationale note (repeatable)")
	return cmd
}

// newWeaveFleetInterviewCmd implements `weave fleet interview [tool…|--all]`:
// the conductor's one-on-one calibration. It resolves each tool's binary +
// version and records its unattended launch contract into a persistent profile
// (~/.bashy/weave/tools/<tool>.json), seeding a first interview from the known
// contracts. This makes the headless launch recipe explicit + discoverable so a
// campaign never bare-launches a tool into its trust/welcome prompt.
func newWeaveFleetInterviewCmd() *cobra.Command {
	var flags weaveOutputFlags
	var all, live bool
	cmd := &cobra.Command{
		Use:   "interview [tool...]",
		Short: "Calibrate tool profiles (launch contract + version); --live verifies the contract still parses",
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			dir, err := weaveToolsDir()
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet interview",
					weavecli.ExitGenericFail, err))
			}
			tools := args
			if all || len(tools) == 0 {
				tools = append([]string(nil), weaveDefaultFleet...)
			}
			now := time.Now()
			profiles := make([]map[string]any, 0, len(tools))
			for _, tool := range tools {
				p, ierr := interviewTool(dir, tool, now, live)
				if ierr != nil {
					return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet interview",
						weavecli.ExitGenericFail, ierr))
				}
				if mode != weavecli.OutputJSON {
					found := "NOT FOUND"
					if p.Bin != "" {
						found = p.Bin
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%-10s %s  %s\n", p.Tool, found, p.Version)
					fmt.Fprintf(cmd.OutOrStdout(), "  launch:  %s %s \"<body>\"\n", p.Tool, strings.Join(p.HeadlessArgs, " "))
					if p.TrustClear != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  trust:   %s\n", p.TrustClear)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  steer:   say=%v  graceful-quit=%v\n", p.SupportsSay, p.SupportsGracefulQuit)
					if p.ContractOK != nil {
						status := "OK"
						if !*p.ContractOK {
							status = "STALE ⚠"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "  contract:%s — %s\n", status, p.ContractNote)
					}
					if p.Notes != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  notes:   %s\n", p.Notes)
					}
					for _, role := range sortedRoleNames(p) {
						r := p.Roles[role]
						if r.Verdict != "" {
							fmt.Fprintf(cmd.OutOrStdout(), "  role %-9s %d/5 — %s\n", role, r.Rating, r.Verdict)
						} else {
							fmt.Fprintf(cmd.OutOrStdout(), "  role %-9s %d/%d pass\n", role, r.Passed, r.Runs)
						}
					}
				}
				profiles = append(profiles, map[string]any{
					"tool": p.Tool, "bin": p.Bin, "version": p.Version,
					"headless_args": p.HeadlessArgs, "trust_clear": p.TrustClear,
					"supports_say": p.SupportsSay, "supports_graceful_quit": p.SupportsGracefulQuit,
				})
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave fleet interview", map[string]any{"profiles": profiles}))
		},
	}
	flags.attach(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "Interview the whole default fleet")
	cmd.Flags().BoolVar(&live, "live", false, "Live-probe the launch contract (run the tool with its flags; catch stale/drifted contracts)")
	return cmd
}

type fleetRow struct {
	// Tool is the executable's registry name. It stays the primary key so
	// every existing consumer of `weave fleet --json` keeps reading what it
	// always read.
	Tool string `json:"tool"`
	// Agent, Model, and Binding are populated when the roster entry named an
	// AGENT rather than a bare tool. Additive: absent for a tool row.
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`   // provider-side id
	Binding string `json:"binding,omitempty"` // tool:model — the capability-matrix key
	// Reason explains an unavailable row. A dangling binding is reported, not
	// silently dropped: an orchestrator that never sees the row cannot learn
	// why it may not assign the agent.
	Reason string `json:"reason,omitempty"`

	Available     bool      `json:"-"`     // assignable only when live smoke evidence exists
	Found         bool      `json:"found"` // binary resolves on PATH
	Path          string    `json:"path,omitempty"`
	CoolingUnit   string    `json:"cooling_until,omitempty"`  // RFC3339 local
	CooldownCause string    `json:"cooldown_cause,omitempty"` // quota-exhausted | cooling-down
	Probed        bool      `json:"probed,omitempty"`
	Capable       bool      `json:"capable,omitempty"` // smoke stdout contained SmokeToken
	ProbedAt      time.Time `json:"probed_at,omitempty"`
	Version       string    `json:"version,omitempty"`
	AuthProbed    bool      `json:"auth_probed,omitempty"`
	Auth          string    `json:"auth,omitempty"`      // ready | needs-login | stale-contract
	AuthNote      string    `json:"auth_note,omitempty"` // why
	AuthHint      string    `json:"auth_hint,omitempty"` // how to fix (needs-login only)

	// launch is the resolved agent, when the entry named one. Unexported: it
	// is machinery, not part of the wire shape.
	launch *weaveAgentLaunch
	// coolingUntil is CoolingUnit as an instant, kept so the human printer
	// can show the full local date+time without re-reading the store.
	coolingUntil time.Time
}

// fleetProbeEntry is one tool's cached capability probe.
type fleetProbeEntry struct {
	Capable  bool      `json:"capable"`
	Version  string    `json:"version"`
	Path     string    `json:"path"`
	ProbedAt time.Time `json:"probed_at"`
}

// weaveFleetRoster returns the roster to report on.
//
// --agents expands to every agent in the registry; otherwise the roster is the
// --fleet list (which may name agents or tools) or the default tool roster.
func weaveFleetRoster(fleetCSV string, agents bool) []string {
	if agents {
		list, _ := fleetCatalog().Agents()
		out := make([]string, 0, len(list))
		for _, a := range list {
			out = append(out, a.Name)
		}
		return out
	}
	if r := parseWeaveAutopilotFleet(fleetCSV); len(r) > 0 {
		return r
	}
	return append([]string(nil), weaveDefaultFleet...)
}

func runWeaveFleet(cmd *cobra.Command, fleetCSV string, probe, auth, agents bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet",
			weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave fleet",
			weavecli.ExitGenericFail, err))
	}

	roster := weaveFleetRoster(fleetCSV, agents)

	now := time.Now()
	cache := loadFleetProbeCache(dir)
	dirty := false
	rows := make([]fleetRow, 0, len(roster))
	for _, name := range roster {
		row, d := fleetRowForEntry(dir, name, now, probe, cache)
		dirty = dirty || d
		if auth && row.Found && row.CoolingUnit == "" && row.Reason == "" {
			row.probeAuth()
		}
		rows = append(rows, row)
	}
	if dirty {
		saveFleetProbeCache(dir, cache)
	}

	if mode == weavecli.OutputJSON {
		tools := make([]map[string]any, len(rows))
		for i, r := range rows {
			m := map[string]any{"tool": r.Tool, "installed": r.Found}
			if r.Agent != "" {
				m["agent"], m["model"], m["binding"] = r.Agent, r.Model, r.Binding
			}
			if r.Reason != "" {
				m["reason"] = r.Reason
			}
			if r.Path != "" {
				m["path"] = r.Path
			}
			if r.CoolingUnit != "" {
				m["cooling_until"] = r.CoolingUnit
			}
			if r.CooldownCause != "" {
				m["cooldown_cause"] = r.CooldownCause
			}
			if r.Probed {
				m["probed"], m["usable"] = true, r.Capable
				m["probed_at"] = r.ProbedAt.Format(time.RFC3339)
				if r.Version != "" {
					m["version"] = r.Version
				}
			}
			if r.AuthProbed {
				m["auth_probed"], m["auth"] = true, r.Auth
				if r.AuthNote != "" {
					m["auth_note"] = r.AuthNote
				}
				if r.AuthHint != "" {
					m["auth_hint"] = r.AuthHint
				}
			}
			tools[i] = m
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave fleet", map[string]any{"tools": tools}))
	}

	for _, r := range rows {
		label := r.Tool
		if r.Agent != "" {
			label = r.Agent
		}
		fmt.Fprintln(cmd.OutOrStdout(), r.statusLine(label))
		if r.Binding != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%-16s   binding: %s → %s\n", "", r.Binding, r.Model)
		}
		if r.AuthProbed {
			switch r.Auth {
			case ReadyOK:
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s   auth: READY — %s\n", "", r.AuthNote)
			case ReadyNeedsAuth:
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s   auth: NEEDS-LOGIN — %s\n", "", r.AuthNote)
				if r.AuthHint != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s         ↳ %s\n", "", r.AuthHint)
				}
			case ReadyStale:
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s   auth: STALE-CONTRACT — %s\n", "", r.AuthNote)
			}
		}
	}
	return ec(emitOK(cmd.OutOrStdout(), mode, "weave fleet", nil))
}

// statusLine is one row's human report. An active cooldown is a DISTINCT
// status (QUOTA-EXHAUSTED / COOLING-DOWN) carrying the full local reset
// date+time — a quota reset can be days out, so a bare clock time (let alone
// "available") would lie about assignability.
func (r fleetRow) statusLine(label string) string {
	switch {
	case r.Reason != "":
		return fmt.Sprintf("%-16s UNAVAILABLE  %s", label, r.Reason)
	case !r.Found:
		return fmt.Sprintf("%-16s NOT FOUND on PATH", label)
	case r.CoolingUnit != "":
		return fmt.Sprintf("%-16s %s until %s — last invocation hit a provider limit; not assignable",
			label, strings.ToUpper(r.CooldownCause), r.coolingUntil.Local().Format("2006-01-02 15:04"))
	case r.Probed && !r.Capable:
		return fmt.Sprintf("%-16s UNUSABLE  smoke output lacked %s (probed %s; %s)", label, fleet.SmokeToken, r.ProbedAt.Local().Format("2006-01-02 15:04"), r.Path)
	case r.Probed:
		return fmt.Sprintf("%-16s USABLE    smoke token confirmed (probed %s; %s)", label, r.ProbedAt.Local().Format("2006-01-02 15:04"), r.Path)
	default:
		return fmt.Sprintf("%-16s installed  (%s; not live-probed)", label, r.Path)
	}
}

// headlessArgs is the flag list an auth probe should launch with: the agent's
// real argv when the row names one, else the tool's seeded contract.
func (r *fleetRow) headlessArgs() ([]string, string) {
	if r.launch != nil {
		hint := ""
		if t, ok := fleetCatalog().Tool(r.Tool); ok {
			hint = t.CLI.Launch.AuthHint
		}
		return r.launch.Args, hint
	}
	p, _ := seededContract(r.Tool)
	return p.HeadlessArgs, p.AuthHint
}

// probeAuth runs the row's real headless invocation on a trivial prompt. For an
// agent that means the model flag is included — a sign-in probe that omitted it
// would not exercise the launch the orchestrator is about to make.
func (r *fleetRow) probeAuth() {
	args, hint := r.headlessArgs()
	status, note := liveProbeReady(r.Tool, args)
	r.AuthProbed, r.Auth, r.AuthNote = true, status, note
	if status == ReadyNeedsAuth {
		r.AuthHint = hint
	}
}

// fleetRowForEntry evaluates one roster entry — an agent or a bare tool.
//
// An agent's assignability is its TOOL's assignability plus its model's. The
// tool half is shared: two agents on one tool cool down together, because the
// throttle belongs to the binary and its provider, not to the binding.
func fleetRowForEntry(dir, name string, now time.Time, probe bool, cache map[string]fleetProbeEntry) (fleetRow, bool) {
	launch, err := weaveResolveAgent(name)
	if err != nil {
		// A dangling binding: report it, never hide it.
		return fleetRow{Tool: name, Agent: name, Reason: err.Error()}, false
	}
	if launch == nil {
		return fleetRowFor(dir, name, now, probe, cache) // a bare tool, exactly as before
	}

	row, dirty := fleetRowForBinary(dir, launch.ToolName, launch.Tool, now, probe, cache)
	row.launch = launch
	row.Agent, row.Model, row.Binding = launch.Nick, launch.Model, launch.Binding()

	// The model half. Structural and offline: a probe that dialed a provider on
	// every preflight would make an offline orchestrator look broken.
	if chk := fleetCatalog().VerifyModel(launch.ModelName, fleet.Probes(nil)); !chk.OK {
		row.Available = false
		row.Reason = "model " + launch.ModelName + ": " + chk.Reason
	}
	return row, dirty
}

// fleetRowFor evaluates one tool's assignability: existence on PATH (always),
// throttle cooldown, and — under probe — a cached capability check. Returns the
// row and whether the probe cache was updated (so the caller can persist it).
// A tool is Available only if its binary resolves AND it is not cooling down.
func fleetRowFor(dir, tool string, now time.Time, probe bool, cache map[string]fleetProbeEntry) (fleetRow, bool) {
	return fleetRowForBinary(dir, tool, tool, now, probe, cache)
}

// fleetRowForBinary is fleetRowFor with the executable named separately. A
// tool's binary need not be its name (cursor runs `cursor-agent`), and looking
// up the name would report a healthy tool as NOT FOUND.
//
// Cooldown and the probe cache stay keyed by the TOOL name, not the binary:
// that is the key `weave start` records a throttle under, and two agents
// sharing a tool must share its cooldown.
func fleetRowForBinary(dir, tool, binary string, now time.Time, probe bool, cache map[string]fleetProbeEntry) (fleetRow, bool) {
	row := fleetRow{Tool: tool}
	dirty := false
	// Existence: does the binary resolve on PATH? (cheap, never cached)
	if p, lookErr := exec.LookPath(binary); lookErr == nil {
		row.Found, row.Path = true, p
	}
	// Cooldown: recorded by the last real invocation that hit a provider
	// quota/rate limit (a throttled `weave start`, or a pair review whose
	// reviewer died on its usage cap). This is what keeps fleet honest about
	// usability, not just presence: a member whose last invocation failed on
	// quota and whose reset has not passed is NEVER "available".
	cooling := false
	if until, cause, ok := toolCooldownStatus(dir, tool); ok && until.After(now) {
		cooling = true
		row.CoolingUnit = until.Local().Format(time.RFC3339)
		row.CooldownCause = cause
		row.coolingUntil = until
	}
	// Liveness (opt-in, cached): only meaningful if the binary exists.
	if probe && row.Found {
		row.Probed = true
		ent, fresh := cache[tool]
		if !fresh || now.Sub(ent.ProbedAt) > fleetProbeTTL || ent.Path != row.Path {
			ent = probeToolCapability(tool, row.Path, now)
			cache[tool] = ent
			dirty = true
		}
		row.Capable, row.Version, row.ProbedAt = ent.Capable, ent.Version, ent.ProbedAt
	}
	row.Available = row.Found && !cooling && row.Probed && row.Capable
	return row, dirty
}

// probeToolCapability runs the tool's declared headless launch with a smoke
// prompt. It judges stdout only: a zero exit carrying an error payload is an
// unusable tool. This is the only place fleet executes a tool, and only under
// --probe.
func probeToolCapability(tool, path string, now time.Time) fleetProbeEntry {
	ent := fleetProbeEntry{Path: path, ProbedAt: now}
	argv, ok := fleetCatalog().SmokeArgv(tool)
	if !ok {
		return ent
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv[0] = path // execute the exact path LookPath resolved on this host.
	out, _ := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	ent.Capable = strings.Contains(string(out), fleet.SmokeToken)
	return ent
}

func fleetProbeCachePath(dir string) string { return filepath.Join(dir, "fleet-probe-cache.json") }

func loadFleetProbeCache(dir string) map[string]fleetProbeEntry {
	m := map[string]fleetProbeEntry{}
	if b, err := os.ReadFile(fleetProbeCachePath(dir)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveFleetProbeCache(dir string, m map[string]fleetProbeEntry) {
	if ensureWeaveQueueDirPath(dir) != nil {
		return
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = weaveWriteFile(fleetProbeCachePath(dir), b, 0o644)
	}
}
