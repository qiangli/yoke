package fleet

import (
	"os"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Entry kinds — the noun a name resolves to.
const (
	KindTool   = "tool"
	KindModel  = "model"
	KindAgent  = "agent"
	KindPerson = "person"
	KindHost   = "host"
	// KindSkill is held by pkg/skills (a folder per entry), which supplies
	// its own Storage; the table knows the kind, not its records.
	KindSkill = "skill"
	// KindCommand is the registered-command noun (`bashy commands add`): a
	// host asset whose FACET is an action (docs/bashy-action-model.md). It is
	// the sixth fleet noun and the only one with no embedded ring — bashy
	// ships the mechanism, never a catalog of commands.
	KindCommand = "command"
	// KindApp is the registered-app noun (`bashy app add`): a local web
	// server tiled by the Apps console. Like commands, no embedded ring.
	KindApp = "app"
	// KindResource is the registered-resource noun (`bashy resource add`):
	// the operator's name for something agents must not use at the same
	// time. A resource carries no claim logic of its own — its record names
	// the claim kind its claims are taken under, and the fleetkinds provider
	// turns the record into that kind's members. Like commands and apps, no
	// embedded ring.
	KindResource = "resource"
	// KindResourceKind is the user-claim-kind noun (`bashy resource kind
	// add`): a record declaring one coord kind — its match rule, domain,
	// TTL, modes and hook commands — so user kinds work wherever builtin
	// kinds do. Like resources, no embedded ring.
	KindResourceKind = "resourcekind"
)

// Tool kind discriminators. The cloudbox Tool registry is shared between
// MCP-style function kits and agentic CLI harnesses; only ToolKindCLI is
// a fleet tool. The others are recognized so they can be skipped by name
// rather than silently mis-parsed.
const (
	ToolKindCLI    = "cli"
	ToolKindFunc   = "func"
	ToolKindWeb    = "web"
	ToolKindSystem = "system"

	// ToolCredentialModelProvider means the harness calls the bound model's
	// provider API directly and therefore needs that provider's credential in
	// its child environment. Subscription-native CLIs such as claude and codex
	// leave this empty because they authenticate through their own login.
	ToolCredentialModelProvider = "model-provider"
)

// Model kind — HOW YOU AUTHENTICATE. Nothing else.
//
// This used to be "the access/billing discriminator" — one field naming two things —
// and it worked only because the two axes happened to travel together in every model we
// had: a seat plan authenticated by an interactive login, an API authenticated by a key
// and billed per token.
//
// z.ai's GLM Coding Plan is the case that separates them: FLAT-RATE BILLING OVER AN API
// KEY. Economically a subscription, operationally a plain HTTP call. There is no value
// of a single enum that can say that.
//
// The tell was already in verify.go, which had to describe BOTH axes in every message
// ("metered api; bills against the vault key", "subscription seat; the CLI
// authenticates interactively"). A field whose every description needs two clauses is
// two fields.
//
// A fourth value (`api-subscription`) would have worked for GLM and then grown as the
// PRODUCT of the axes — a metered vendor CLI needs a fifth, a flat-rate local pool a
// sixth. Same mistake in a new costume. So: Kind is auth, Billing is billing, and each
// names one thing.
const (
	ModelKindSubscription = "subscription" // interactive login; the CLI authenticates on the host
	ModelKindAPI          = "api"          // an API key, named by APIKeyRef
	ModelKindLocal        = "local"        // no credential; pooled local inference via the outpost
)

// Model billing — HOW YOU PAY. Orthogonal to Kind.
//
// Optional. When absent it is DERIVED from Kind by Model.BillingMode(), which reproduces
// the old collapsed behaviour exactly — so every model written before this field existed
// keeps its meaning, and there is no migration.
// The values differ in WHAT HAPPENS WHEN THE QUOTA RUNS OUT, and that is the part that
// matters most — because the two failure modes are opposites:
//
//	flat              -> the agent STOPS WORKING. A reliability event. Loud.
//	flat_then_metered -> the agent KEEPS WORKING AND STARTS CHARGING YOU. A cost event. SILENT.
//
// The second is the dangerous one. An unattended fleet run that exhausts a subscription
// seat does not fail — it quietly moves onto pay-as-you-go and you find out on the
// invoice. That is why it is a first-class value and not a footnote on `flat`.
const (
	BillingMetered = "metered" // per token, always. The next token costs money.
	BillingFlat    = "flat"    // a prepaid seat with a HARD quota. Exhausted -> blocked until it resets.
	BillingFree    = "free"    // your own hardware. No bill at all.

	// BillingFlatThenMetered is a prepaid seat that FALLS BACK TO PER-TOKEN BILLING once
	// the quota is gone, instead of blocking. Anthropic Max/Pro and Codex work this way.
	//
	// At the margin, below quota, it prices exactly like `flat`. The difference is not
	// the price — it is that overrunning does not fail, it BILLS. Routing treats it as
	// flat; `models verify` says the quiet part out loud.
	BillingFlatThenMetered = "flat_then_metered"
)

// Model source — where the row came from, not how it is billed.
const (
	ModelSourceCloud = "cloud"
	ModelSourceLocal = "local"
)

// PromptToken, ModelToken, WorkspaceToken and SessionToken are the
// launch-template placeholders.
const (
	PromptToken    = "{prompt}"
	ModelToken     = "{model}"
	WorkspaceToken = "{workspace}"
	SessionToken   = "{session}" // the current session id, for a context-inheriting fork
	// GOCacheToken is the per-run managed build cache a sandboxed worker must
	// be allowed to write. Only the orchestrator knows it, so it survives
	// rendering as a quoted list element (`,"{gocache}"`) until BindGOCache.
	GOCacheToken = "{gocache}"
)

// BindGOCache fills GOCacheToken in s with cache, or drops the whole list
// element when cache is empty so the result stays well-formed.
func BindGOCache(s, cache string) string {
	elem := `,"` + GOCacheToken + `"`
	if cache == "" {
		return strings.ReplaceAll(s, elem, "")
	}
	return strings.ReplaceAll(s, GOCacheToken, cache)
}

// Tool is an agentic CLI harness.
//
// The canonical YAML keys are `name:` and `kind:`. Assets written before
// that was settled spell them `kit:` and `type:`; both are accepted on
// parse and neither is emitted. See parse.go.
type Tool struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"canonical registry name"`
	Kind            string   `yaml:"kind" json:"kind" doc:"tool kind: cli, func, web, or system"` // cli | func | web | system
	Aliases         []string `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"alternate accepted names"`
	Display         string   `yaml:"display,omitempty" json:"display,omitempty" doc:"human-facing label"`
	// Hidden keeps a tool in the registry (still detected, still resolvable by
	// explicit name) but omits it from `bashy tool` list/help unless --all.
	Hidden bool    `yaml:"hidden,omitempty" json:"hidden,omitempty" doc:"omit the tool from default listings"`
	CLI    ToolCLI `yaml:"cli,omitempty" json:"cli" doc:"command-line harness settings"`
	Quirks string  `yaml:"quirks,omitempty" json:"quirks,omitempty" doc:"known tool-specific behavior"`

	// Harness scores the capabilities a tool governs regardless of the
	// model behind it (operability, shell, tool-use, isolation). The
	// capability matrix reads these as priors.
	Harness map[string]float64 `yaml:"harness,omitempty" json:"harness,omitempty" doc:"harness capability priors"`

	// Commands exposes this tool's own vendor features (slash commands,
	// skills, subcommands) as named bashy services — the inverse of cligw,
	// which strips them. Data only; see toolcommand.go and
	// docs/tool-commands-design.md (Sprint #324).
	Commands []ToolCommand `yaml:"commands,omitempty" json:"commands,omitempty" doc:"vendor slash commands exposed as bashy tool commands, keyed by canonical cross-tool name"`

	// Integration declares how the tool takes skills, instructions and MCP
	// servers, so bashy can enable its skills in any tool from data
	// (integration.go).
	Integration ToolIntegration `yaml:"integration,omitempty" json:"integration" doc:"how the tool takes skills, instruction files and MCP servers"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// Tool command modes.
const (
	ToolCommandPrint = "print" // one-shot through the exec template (or the command's own exec)
	ToolCommandTUI   = "tui"   // typed into a steered interactive session (steer_exec)
)

// ToolCommand is one vendor feature of a tool exposed as a bashy command
// (`<tool>:<name>`). Name is a CANONICAL cross-tool name — every tool
// registers the same feature under the same name (CanonicalToolCommands) —
// and Slash is this tool's own spelling of it.
type ToolCommand struct {
	Name       string            `yaml:"name" json:"name" doc:"canonical cross-tool command name (plan, review, deep-research, ...); unique per tool"`
	Effects    []string          `yaml:"effects,omitempty" json:"effects,omitempty" doc:"declared effect atoms for guarded callers"`
	Slash      string            `yaml:"slash" json:"slash" doc:"the tool-specific line to send; {args} is replaced by the caller's text"`
	Mode       string            `yaml:"mode" json:"mode" doc:"print (one-shot via the exec template) or tui (steered session via steer_exec)"`
	Capability string            `yaml:"capability,omitempty" json:"capability,omitempty" doc:"capability tag for --capability routing"`
	Timeout    string            `yaml:"timeout,omitempty" json:"timeout,omitempty" doc:"Go duration bounding the whole command; empty = runner default"`
	Output     string            `yaml:"output,omitempty" json:"output,omitempty" doc:"turn (final message, default), transcript, or file:<glob> (artifacts in the workdir)"`
	Exec       string            `yaml:"exec,omitempty" json:"exec,omitempty" doc:"print only: argv template used instead of cli.launch.exec, for a feature that is a subcommand ({args}, {model}, {workspace})"`
	Steps      []ToolCommandStep `yaml:"steps,omitempty" json:"steps,omitempty" doc:"tui only: scripted frames after the slash line"`
	Quit       string            `yaml:"quit,omitempty" json:"quit,omitempty" doc:"tui only: line that ends the session; empty = the tool's graceful quit"`
}

// ToolCommandStep is one scripted TUI frame. Exactly one field is set.
type ToolCommandStep struct {
	Say      string `yaml:"say,omitempty" json:"say,omitempty" doc:"type a line into the session"`
	Key      string `yaml:"key,omitempty" json:"key,omitempty" doc:"send one named key verbatim (esc, enter, tab, ctrl-c, ...)"`
	WaitIdle string `yaml:"wait_idle,omitempty" json:"wait_idle,omitempty" doc:"wait until the session is quiet for this Go duration"`
}

type ToolCLI struct {
	Binary   string        `yaml:"binary,omitempty" json:"binary,omitempty" doc:"executable to run"`
	Versions []ToolVersion `yaml:"versions,omitempty" json:"versions,omitempty" doc:"known downloadable versions"`
	// Managed pins a binmgr-managed install of the vendor's published artifact
	// (version + per-platform digest). When present it is the ONLY binary a
	// launch uses; see managed.go for the override and self-update rules.
	Managed *ToolManaged `yaml:"managed,omitempty" json:"managed,omitempty" doc:"pinned binmgr-managed install: vendor artifact per platform, verified by digest"`
	Linux   ToolLinux    `yaml:"linux,omitempty" json:"linux,omitempty" doc:"Linux image installation recipe, separate from host installation"`
	Launch  ToolLaunch   `yaml:"launch,omitempty" json:"launch" doc:"headless and interactive launch contract"`
}

// ToolLinux describes how a CLI is installed in a sealed Linux image.
// It is intentionally separate from ToolVersion.Install, which is a host
// installation recipe.
type ToolLinux struct {
	Install  string   `yaml:"install,omitempty" json:"install,omitempty" doc:"shell command run in the Linux image; {version} is replaced by cli.versions.version"`
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty" doc:"apt packages required before installation"`
	Binary   string   `yaml:"binary,omitempty" json:"binary,omitempty" doc:"binary path or name in the Linux image"`
}

type ToolVersion struct {
	Version  string `yaml:"version,omitempty" json:"version,omitempty" doc:"version identifier"`
	Download string `yaml:"download,omitempty" json:"download,omitempty" doc:"download location"`
	Install  string `yaml:"install,omitempty" json:"install,omitempty" doc:"installation command"`
}

// ToolLaunch is how the orchestrator invokes a tool headlessly.
type ToolLaunch struct {
	// Exec is the argv template. {prompt} is replaced by the task text and
	// {model} by the bound model's upstream id. When no model is bound,
	// {model} and the flag token immediately preceding it are dropped, so
	// a template with a model flag degrades exactly to one without.
	Exec string `yaml:"exec,omitempty" json:"exec,omitempty" doc:"headless argv template"`
	// Warm declares how cligw may start a one-shot process before its prompt is
	// known. Empty and "cold" both defer process creation until the request.
	// stdin-stream-json sends one tool-specific JSON user message, stdin sends
	// the prompt bytes directly, and acp is reserved for the ACP transport.
	Warm string `yaml:"warm,omitempty" json:"warm,omitempty" doc:"prewarm transport: stdin-stream-json, acp, stdin, or cold"`
	// Credential declares how this harness authenticates a bound model.
	// "model-provider" grants only the credential named by Model.APIKeyRef, or
	// by Model.Provider when no explicit key reference exists. Values remain in
	// the launcher environment; this field carries names and policy only.
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty" doc:"credential policy for bound models"`
	// CredentialEnv maps a model api_key_ref to the environment variable this
	// CLI expects. The launcher finds the value under the ordinary names for the
	// reference, then projects it under this declared name after scrubbing the
	// child environment. This keeps provider-specific spelling in fleet assets.
	CredentialEnv map[string]string `yaml:"credential_env,omitempty" json:"credential_env,omitempty" doc:"api_key_ref to CLI credential environment variable aliases"`
	// KeyEnv names the environment variables that receive the bound model's
	// credential whatever its api_key_ref — the protocol's key name (an
	// OpenAI-compatible CLI reads OPENAI_API_KEY for every endpoint). A
	// CredentialEnv entry for the ref still wins. Names only; the child
	// firewall copies the value after scrubbing.
	KeyEnv []string `yaml:"key_env,omitempty" json:"key_env,omitempty" doc:"environment variables that receive the bound model's credential"`
	// Env is KEY=VALUE pairs set on every launch. Values may carry the bound
	// model's {model} (its tool-specific id), {base_url}, {base_url_origin}
	// (scheme://host[:port]), {base_url_path} and {state_dir} (a bashy-owned
	// directory private to the tool:model binding); a pair whose placeholder has
	// no value for this launch is dropped, never set empty. A tool that takes
	// its model from the environment declares {model} here instead of argv.
	Env []string `yaml:"env,omitempty" json:"env,omitempty" doc:"KEY=VALUE pairs set on every launch ({model}, {base_url}, {base_url_origin}, {base_url_path}, {state_dir})"`
	// Setup is a Bash# snippet (classic shell is valid Bash#) bashy runs
	// immediately before every launch of
	// this tool (headless, steered, ACP, weave, cligw alike), in the tool's own
	// child environment and working directory: write the provider config the
	// CLI reads, select its default model. Placeholders are the Env ones,
	// shell-quoted; credentials are referenced as $VARS from KeyEnv or
	// CredentialEnv, never as placeholders. A failing setup fails the launch.
	Setup string `yaml:"setup,omitempty" json:"setup,omitempty" doc:"shell snippet run before every launch ({model}, {base_url}, ... shell-quoted)"`
	// MinContext is the smallest context window (tokens) this CLI accepts;
	// a binding to a model whose declared context_length is below it is
	// refused at launch with a clear message instead of failing inside the
	// tool. 0 means no minimum. A model with no declared length is not
	// checked: the tool's own error remains the authority there.
	MinContext int64 `yaml:"min_context,omitempty" json:"min_context,omitempty" doc:"smallest model context window (tokens) the CLI accepts"`
	// WorkspaceArg is an optional argv fragment that binds the launched tool to
	// the orchestrator's allocated workspace. {workspace} is replaced by that
	// absolute path. It is rendered immediately after the binary, before the
	// exec template's model and prompt arguments. Tools that do not declare it
	// retain their existing argv exactly.
	WorkspaceArg string `yaml:"workspace_arg,omitempty" json:"workspace_arg,omitempty" doc:"argv fragment binding a workspace"`
	// WorkspacePreflightExec is an optional read-only launch template used to
	// ask the tool which PWD/project directory it selected. The launcher supplies
	// a reporting prompt and refuses to start the source-writing invocation
	// unless the reported path equals the allocated workspace.
	WorkspacePreflightExec string `yaml:"workspace_preflight_exec,omitempty" json:"workspace_preflight_exec,omitempty" doc:"read-only workspace verification template"`
	// VersionProbeExec is an optional provider-declared, read-only command used
	// by fleet capability probing instead of assuming every CLI accepts --version.
	VersionProbeExec string `yaml:"version_probe_exec,omitempty" json:"version_probe_exec,omitempty" doc:"version-probe argv template"`
	// PromptPosition records where the prompt goes for consumers that
	// cannot read the template (cloudbox conductor). Advisory here: the
	// {prompt} placeholder is authoritative.
	PromptPosition string `yaml:"prompt_position,omitempty" json:"prompt_position,omitempty" doc:"declared prompt position"`
	// TrustPreseed names a config file the host must pre-seed so the CLI
	// does not no-op on a first-run trust prompt.
	TrustPreseed string       `yaml:"trust_preseed,omitempty" json:"trust_preseed,omitempty" doc:"config path preseeded for workspace trust"`
	Watchdog     ToolWatchdog `yaml:"watchdog,omitempty" json:"watchdog" doc:"runtime resource limits"`

	// SupportsSay marks a tool that CAN be steered mid-run — a capability fact
	// about the tool, MEASURED (pkg/agentpty/steer_live_test.go), not asserted.
	SupportsSay bool `yaml:"supports_say,omitempty" json:"supports_say,omitempty" doc:"whether live steering is supported"`

	// SteerInterrupt marks a tool whose TUI will not take a typed line until its
	// current turn ends, so `bashy chat steer` sends ESC before the line. Only for
	// a CLI that needs it: a TUI that merely drops a busy-time Enter is covered by
	// agentpty's second Enter (Muse Code 1.3 turned out to be that case).
	SteerInterrupt bool `yaml:"steer_interrupt,omitempty" json:"steer_interrupt,omitempty" doc:"interrupt (ESC) before a steer line"`

	// ACPExec is the argv template that launches this tool as an ACP AGENT
	// speaking JSON-RPC on stdio. Empty means the tool does not speak ACP and
	// the launcher falls to the next rung.
	//
	// NO {prompt} and NO {model}. The prompt travels in the ACP session rather
	// than argv, which is what gives the transport a real turn boundary. The
	// model is fixed by the binding before the launch: ACP carries no
	// model-selection call, and a bound model refuses the ACP rung outright
	// (see agentlaunch.ACPArgv). A {model} token here renders LITERALLY.
	ACPExec string `yaml:"acp_exec,omitempty" json:"acp_exec,omitempty" doc:"ACP agent argv template"`

	// EventsArg is how this tool is told to stream STRUCTURED EVENTS, if it can.
	//
	// This is the difference between a first-party harness and a third-party one,
	// and it is not cosmetic. Without it, bashy decides a turn has ended by
	// WATCHING FOR SILENCE — 25 seconds of no output (see chat.Session.WaitIdle).
	// That heuristic is wrong in both directions: an agent that pauses to think
	// looks finished, and an agent that renders a spinner never does. Every turn
	// also pays the 25 seconds on its way out, which is why `meet --steerable` is
	// a flag and not the default.
	//
	// A tool that declares this gets a real boundary instead: it says `turn.end`,
	// and bashy believes it, because it is a fact the agent reported rather than a
	// silence bashy interpreted.
	//
	// Template with one token: {path}. e.g. `--events {path}`.
	// The events are NDJSON, one object per line, with at minimum:
	//     {"type":"turn.start"} {"type":"tool.call"} {"type":"turn.end", ...}
	EventsArg string `yaml:"events_arg,omitempty" json:"events_arg,omitempty" doc:"structured-event side-channel argument"`

	// EventsStdout is the same capability for tools that stream on STDOUT
	// rather than into a file bashy names.
	//
	// EventsArg above was written for ycode, which takes a path and writes
	// NDJSON there. Every third-party tool measured on the wire does the
	// opposite — claude, codex and agy each stream to stdout and accept no path
	// at all — so declaring EventsArg for them would render a flag that does not
	// exist. Two fields, because they are two different plumbing paths: one
	// opens a side channel, the other means stdout is no longer a transcript to
	// scrape but a stream to parse.
	//
	// Fixed argv, no {path} token. Measured 2026-07-31:
	//
	//	claude   -p {prompt} --output-format stream-json --verbose
	//	codex    exec --json
	//	agy      -p {prompt} --output-format stream-json
	//
	// THESE ARE PRINT-MODE FLAGS, AND THAT BOUNDS WHAT THEY BUY. All three
	// belong to the one-shot Exec path (`-p`, `exec`) — claude states outright
	// that stream-json works only with --print — and none is available on
	// SteerExec, the bare interactive TUI.
	//
	// So they do NOT remove the 25-second silence tax, and it would be wrong to
	// wire them expecting that. The tax lives on the STEERING path (coach,
	// foreman, meet, herald all call Session.WaitIdle), where a session persists
	// across turns and these flags cannot be passed. A one-shot already has an
	// exact boundary: the process exits.
	//
	// What they buy instead is the fleet-evidence property — a tool call, a
	// stop_reason and an error status as STRUCTURED FACTS rather than lines
	// scraped back out of a terminal and guessed at. That is worth having, and
	// it is a different thing from turn detection.
	EventsStdout string `yaml:"events_stdout,omitempty" json:"events_stdout,omitempty" doc:"structured-event stdout arguments"`

	// EventsDone declares how THIS tool spells "the turn ended".
	//
	// The EventsArg contract documented `{"type":"turn.end"}` and no third-party
	// tool says that. Measured on the wire, the same fact has three spellings —
	// codex `type: turn.completed`, claude `type: result`, agy `event: result` —
	// and even the KEY differs, so a matcher that assumed `type` would silently
	// never fire on agy. Silently, because a boundary that never arrives is
	// indistinguishable from a tool that is still thinking: the reader would
	// fall back to the 25-second silence tax it was trying to escape, and
	// nothing would report that the declaration was wrong.
	//
	// Declared per tool rather than inferred, for the reason this package
	// declares everything else: a guess that happens to work is a guess that
	// breaks on the next release with nobody watching.
	EventsDone EventsDone `yaml:"events_done,omitempty" json:"events_done,omitempty" doc:"terminal-event matcher"`

	// EventsOutcome locates the VERDICT inside that terminal event.
	//
	// The exit code is not it. "All three harnesses EXITED 0 WHEN THEY FAILED"
	// is recorded in the umbrella's own notes, so a caller reading the status
	// learns nothing and reports success. The stream carries what the exit does
	// not. See eventsoutcome.go for why only SUCCESS is declarable.
	EventsOutcome EventsOutcome `yaml:"events_outcome,omitempty" json:"events_outcome,omitempty" doc:"terminal-event success rule"`

	// EventsFail names the event kinds that mean the turn FAILED, so a launcher
	// can turn a tool's "exit 0 with an error event" into a non-zero exit.
	// Declared only from a MEASURED failure line (see eventsfail.go).
	EventsFail EventsDone `yaml:"events_fail,omitempty" json:"events_fail,omitempty" doc:"event kinds that mean the turn failed, whatever the exit code"`

	// SteerExec is the argv template that ACTUALLY accepts steering, and it is
	// usually NOT Exec.
	//
	// A headless one-shot has nothing to steer: `codex exec` and `agy -p` run the
	// prompt and exit. Steering needs the tool's interactive session — bare `codex`,
	// or `agy -i` ("run an initial prompt interactively and CONTINUE the session").
	//
	// Two templates, because the choice is a real trade. Exec gives a clean captured
	// answer (stdout and stderr stay apart on a pipe). SteerExec gives a session you
	// can interrupt, at the cost of a pty that merges the tool's chrome into the
	// transcript. A launcher picks by what it needs; the registry refuses to pretend
	// one launch does both.
	SteerExec string `yaml:"steer_exec,omitempty" json:"steer_exec,omitempty" doc:"interactive steerable argv template"`

	// ForkExec is the argv template that FORKS the tool's current session — a new,
	// independent session that inherits the live transcript — instead of starting
	// fresh. {session} = the current session id, {prompt} = the directive, {model}
	// = the model. This is what makes `delegate self` a true context-inheriting
	// fork ("delegate to yourself, no re-briefing"). ONLY a tool with a genuine
	// HEADLESS, NON-mutating fork declares this: claude has one
	// (`--resume <id> --fork-session -p`), codex does NOT — its headless `resume`
	// APPENDS to the parent thread, which would corrupt the steward's own session,
	// so codex has no ForkExec and `delegate self` falls back to a fresh instance.
	ForkExec string `yaml:"fork_exec,omitempty" json:"fork_exec,omitempty" doc:"context-inheriting fork argv template"`
	// SessionEnv names the env var(s) that carry this tool's current session id
	// when it drives a subprocess (e.g. CLAUDE_CODE_SESSION_ID). First non-empty
	// wins. Without a readable session id, a ForkExec that needs {session} cannot
	// fire, and delegate self falls back to a fresh instance.
	SessionEnv []string `yaml:"session_env,omitempty" json:"session_env,omitempty" doc:"environment variables carrying the session id"`

	// SupportsGracefulQuit marks a tool that exits cleanly on a quit signal.
	SupportsGracefulQuit bool `yaml:"supports_graceful_quit,omitempty" json:"supports_graceful_quit,omitempty" doc:"whether a quit signal exits cleanly"`
	// TrustClear is the steering input that clears a trust prompt.
	TrustClear string `yaml:"trust_clear,omitempty" json:"trust_clear,omitempty" doc:"input that clears a trust prompt"`
	// AuthHint explains an interactive sign-in the tool needs before it
	// can run headless at all.
	AuthHint string `yaml:"auth_hint,omitempty" json:"auth_hint,omitempty" doc:"interactive authentication guidance"`
	// Notes is the free-text launch contract commentary.
	Notes string `yaml:"notes,omitempty" json:"notes,omitempty" doc:"launch contract notes"`
	// EnvMarkers are environment variables whose presence identifies this
	// tool as the one currently running.
	EnvMarkers []string `yaml:"env_markers,omitempty" json:"env_markers,omitempty" doc:"environment variables identifying the tool"`
}

type ToolWatchdog struct {
	MaxRuntime string `yaml:"max_runtime,omitempty" json:"max_runtime,omitempty" doc:"maximum run duration"`
	MemLimit   string `yaml:"mem_limit,omitempty" json:"mem_limit,omitempty" doc:"maximum memory use"`
}

// IsCLI reports whether this tool is an agentic CLI — the only tool kind
// the fleet drives. A missing kind means an old asset that predates the
// discriminator; those were all function kits, so absence is not cli.
func (t Tool) IsCLI() bool { return t.Kind == ToolKindCLI }

// Argv renders the launch template. modelID is the bound model's upstream
// id ("" when the tool must choose its own model); prompt is the task
// text. When modelID is empty, {model} and any flag token immediately
// before it are dropped.
//
// Tokens are whitespace-separated: launch templates are flag lists, never
// shell. A template that needs quoting is a template in the wrong place.
func (t Tool) Argv(modelID, prompt string) []string {
	return t.ArgvWithWorkspace("", modelID, prompt)
}

// ArgvWithWorkspace renders the headless launch and, when the tool declares a
// workspace binding, inserts it directly after the binary. Passing an empty
// workspace preserves the historical template rendering.
func (t Tool) ArgvWithWorkspace(workspace, modelID, prompt string) []string {
	return t.renderLaunch(t.CLI.Launch.Exec, workspace, modelID, "", prompt)
}

func (t Tool) renderLaunch(tmpl, workspace, modelID, session, prompt string) []string {
	fields := strings.Fields(tmpl)
	out := make([]string, 0, len(fields)+1)
	for i, f := range fields {
		if i == 1 && workspace != "" {
			for _, wf := range strings.Fields(t.CLI.Launch.WorkspaceArg) {
				wf = strings.ReplaceAll(wf, WorkspaceToken, workspace)
				if workspace != WorkspaceToken {
					wf = BindGOCache(wf, "")
				}
				out = append(out, wf)
			}
		}
		switch f {
		case ModelToken:
			if modelID != "" {
				out = append(out, modelID)
			} else if n := len(out); n > 0 && strings.HasPrefix(out[n-1], "-") {
				out = out[:n-1] // drop the orphaned flag
			}
		case PromptToken:
			out = append(out, prompt)
		case SessionToken:
			out = append(out, session)
		case WorkspaceToken:
			out = append(out, workspace)
		default:
			// A token carrying {model} inside it (provider/{model}) renders
			// the model in place; unbound, it goes the way a bare {model}
			// does, taking its orphaned flag with it.
			if strings.Contains(f, ModelToken) {
				if modelID == "" {
					if n := len(out); n > 0 && strings.HasPrefix(out[n-1], "-") {
						out = out[:n-1]
					}
					continue
				}
				f = strings.ReplaceAll(f, ModelToken, modelID)
			}
			out = append(out, strings.ReplaceAll(f, WorkspaceToken, workspace))
		}
	}
	return out
}

// WorkspacePreflightArgv renders the tool-declared, read-only workspace
// reporting command. It reports false when no command is declared.
func (t Tool) WorkspacePreflightArgv(workspace, modelID, prompt string) ([]string, bool) {
	if strings.TrimSpace(t.CLI.Launch.WorkspacePreflightExec) == "" {
		return nil, false
	}
	return t.renderLaunch(t.CLI.Launch.WorkspacePreflightExec, workspace, modelID, "", prompt), true
}

// VersionProbeArgv renders the provider-declared capability probe. Tools with
// no declaration retain the universal --version convention.
func (t Tool) VersionProbeArgv() []string {
	if strings.TrimSpace(t.CLI.Launch.VersionProbeExec) == "" {
		return []string{t.CLI.Binary, "--version"}
	}
	return t.renderLaunch(t.CLI.Launch.VersionProbeExec, "", "", "", "")
}

// TakesModel reports whether the launch template can select a model. A
// tool without a {model} placeholder cannot: binding it to a model is a
// label, not a selection.
func (t Tool) TakesModel() bool {
	if strings.Contains(t.CLI.Launch.Exec, ModelToken) {
		return true
	}
	for _, kv := range t.CLI.Launch.Env {
		if strings.Contains(kv, ModelToken) {
			return true
		}
	}
	return strings.Contains(t.CLI.Launch.Setup, ModelToken)
}

// ModelViaLaunch reports whether the tool receives its bound model through
// the launch environment or the setup hook rather than its argv. Such a tool
// can carry a binding over ACP, whose launch has no model flag.
func (t Tool) ModelViaLaunch() bool {
	if strings.Contains(t.CLI.Launch.Setup, ModelToken) {
		return true
	}
	for _, kv := range t.CLI.Launch.Env {
		if strings.Contains(kv, ModelToken) {
			return true
		}
	}
	return false
}

// SetupScript renders the tool's setup snippet for one launch, every
// placeholder value single-quoted for the shell ("" when the tool declares
// no setup).
func (t Tool) SetupScript(v LaunchVars) string {
	s := strings.TrimSpace(t.CLI.Launch.Setup)
	if s == "" {
		return ""
	}
	var pairs []string
	for tok, val := range v.tokens() {
		pairs = append(pairs, tok, shellQuote(val))
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// UsesStateDir reports whether the launch env or setup refers to {state_dir}.
func (t Tool) UsesStateDir() bool {
	if strings.Contains(t.CLI.Launch.Setup, StateDirToken) {
		return true
	}
	for _, kv := range t.CLI.Launch.Env {
		if strings.Contains(kv, StateDirToken) {
			return true
		}
	}
	return false
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Launch-env placeholders filled from the bound model's endpoint, plus the
// bashy-owned private directory a tool's setup writes its config into.
const (
	BaseURLToken       = "{base_url}"
	BaseURLOriginToken = "{base_url_origin}"
	BaseURLPathToken   = "{base_url_path}"
	StateDirToken      = "{state_dir}"
)

// LaunchVars are the per-launch values the env and setup placeholders take.
// StateDir is a bashy-owned directory private to one tool:model binding, so a
// setup never rewrites the user's own configuration of the same CLI.
type LaunchVars struct {
	Model    string // the tool-specific model id
	BaseURL  string // the bound model's endpoint
	StateDir string // bashy-owned per-binding config dir
}

func (v LaunchVars) tokens() map[string]string {
	origin, path := splitBaseURL(v.BaseURL)
	return map[string]string{
		ModelToken:         v.Model,
		BaseURLToken:       v.BaseURL,
		BaseURLOriginToken: origin,
		BaseURLPathToken:   path,
		StateDirToken:      v.StateDir,
	}
}

// LaunchEnv renders the tool's launch env for one launch (empty LaunchVars
// fields mean no model is bound, or it has no endpoint). A pair whose placeholder has no
// value is dropped rather than set empty, so a tool's own default survives.
func (t Tool) LaunchEnv(v LaunchVars) []string {
	if len(t.CLI.Launch.Env) == 0 {
		return nil
	}
	vals := v.tokens()
	var out []string
	for _, kv := range t.CLI.Launch.Env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		drop := false
		for tok, v := range vals {
			if strings.Contains(val, tok) {
				if v == "" {
					drop = true
					break
				}
				val = strings.ReplaceAll(val, tok, v)
			}
		}
		if !drop {
			out = append(out, key+"="+val)
		}
	}
	return out
}

// splitBaseURL splits an endpoint into scheme://host[:port] and its path
// ("/api/coding/paas/v4"); both are "" when baseURL is not an absolute URL.
func splitBaseURL(baseURL string) (origin, path string) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(baseURL), "://")
	if !ok || scheme == "" || rest == "" {
		return "", ""
	}
	host, p, _ := strings.Cut(rest, "/")
	if host == "" {
		return "", ""
	}
	if p != "" {
		path = "/" + strings.TrimRight(p, "/")
	}
	return scheme + "://" + host, path
}

// CredentialRefFor returns the single credential reference this tool needs to
// invoke m. An explicit model key always wins. A direct-provider harness may
// derive the conventional key name from the provider; subscription-native
// tools receive nothing.
func (t Tool) CredentialRefFor(m Model) string {
	if strings.TrimSpace(m.APIKeyRef) != "" {
		return m.APIKeyRef
	}
	if t.CLI.Launch.Credential == ToolCredentialModelProvider {
		return m.Provider
	}
	return ""
}

// ModelFlag is the flag token that carries the model — the token immediately
// before {model} in the launch template, when it is a flag.
//
// A caller that already holds an argv from somewhere else (a self-healed tool
// profile, say) needs the flag's spelling to add the model to it. It returns
// "" when the template positions the model without a flag, and such a template
// can only be rendered whole, by Argv.
func (t Tool) ModelFlag() string {
	fields := strings.Fields(t.CLI.Launch.Exec)
	for i, f := range fields {
		if f == ModelToken && i > 0 && strings.HasPrefix(fields[i-1], "-") {
			return fields[i-1]
		}
	}
	return ""
}

// Binary is the executable to run: the declared one, else the tool's name.
func (t Tool) Binary() string {
	if t.CLI.Binary != "" {
		return t.CLI.Binary
	}
	return t.Name
}

// IsPinned reports whether the tool's binary is set to a path rather than a bare name.
func (t Tool) IsPinned() bool {
	return isPathLike(strings.TrimSpace(t.CLI.Binary))
}

// MissingHelpers reports any known helper executables that are missing from a
// pinned tool's directory, along with a recommended fix command.
func (t Tool) MissingHelpers() ([]string, string) {
	return ToolMissingHelpers(t)
}

// SteerArgvPrefix renders the STEERABLE launch — the interactive session, not the
// headless one-shot.
//
// Reports false when the tool has no steer_exec, i.e. it has no session to open.
// A caller that wants to interrupt an agent must be told that plainly rather than
// handed a one-shot that will exit before the first steer arrives.
func (t Tool) SteerArgvPrefix(modelID string) ([]string, bool) {
	return t.SteerArgvPrefixWithWorkspace("", modelID)
}

// SteerArgvPrefixWithWorkspace is SteerArgvPrefix with an optional declared
// workspace binding rendered immediately after the binary.
func (t Tool) SteerArgvPrefixWithWorkspace(workspace, modelID string) ([]string, bool) {
	tmpl := t.CLI.Launch.SteerExec
	if strings.TrimSpace(tmpl) == "" {
		return nil, false
	}
	// The steer template may carry {prompt} (agy -i takes an opening prompt) or
	// not (codex/opencode open an empty session). Both are legal; the caller
	// appends the prompt only when the template asked for it.
	argv := t.renderLaunch(tmpl, workspace, modelID, "", PromptToken)
	out := argv[1:]
	for i, arg := range out {
		if arg == PromptToken {
			out = append(out[:i], out[i+1:]...)
			break
		}
	}
	return out, true
}

// SteerTakesPrompt reports whether the steerable launch accepts an opening prompt
// on the command line (agy -i does; codex and opencode open an empty session).
func (t Tool) SteerTakesPrompt() bool {
	return strings.Contains(t.CLI.Launch.SteerExec, PromptToken)
}

// ArgvPrefix renders everything between the binary and the prompt, for
// launchers that append the prompt themselves.
//
// It reports false when the template has no {prompt}, or when {prompt} is
// not the final token. Both cases mean the launcher cannot simply append —
// and quietly appending anyway would hand the task text to the wrong flag.
func (t Tool) ArgvPrefix(modelID string) ([]string, bool) {
	return t.ArgvPrefixWithWorkspace("", modelID)
}

func (t Tool) ArgvPrefixWithWorkspace(workspace, modelID string) ([]string, bool) {
	if t.CLI.Launch.Exec == "" || !strings.Contains(t.CLI.Launch.Exec, PromptToken) {
		return nil, false
	}
	argv := t.ArgvWithWorkspace(workspace, modelID, PromptToken)
	if len(argv) < 2 || argv[len(argv)-1] != PromptToken {
		return nil, false
	}
	return argv[1 : len(argv)-1], true
}

// CanFork reports whether the tool declares a native context-inheriting fork.
func (t Tool) CanFork() bool { return strings.TrimSpace(t.CLI.Launch.ForkExec) != "" }

// CurrentSession returns the tool's current session id from the first SessionEnv
// var that is set, or "" — the id needed to fork THIS session rather than start
// a fresh one.
func (t Tool) CurrentSession() string {
	for _, k := range t.CLI.Launch.SessionEnv {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// ForkArgv renders the ForkExec template. Like Argv, but also substitutes
// {session} with the current session id.
func (t Tool) ForkArgv(modelID, session, prompt string) []string {
	return t.ForkArgvWithWorkspace("", modelID, session, prompt)
}

// ForkArgvWithWorkspace is ForkArgv with an optional declared workspace
// binding rendered immediately after the binary.
func (t Tool) ForkArgvWithWorkspace(workspace, modelID, session, prompt string) []string {
	return t.renderLaunch(t.CLI.Launch.ForkExec, workspace, modelID, session, prompt)
}

// ForkArgvPrefix is ArgvPrefix for the fork template: the argv between the binary
// and the trailing {prompt}, with {session}/{model} already substituted. Returns
// false when the tool declares no ForkExec (delegate self then falls back).
func (t Tool) ForkArgvPrefix(modelID, session string) ([]string, bool) {
	return t.ForkArgvPrefixWithWorkspace("", modelID, session)
}

// ForkArgvPrefixWithWorkspace is ForkArgvPrefix with an optional declared
// workspace binding rendered immediately after the binary.
func (t Tool) ForkArgvPrefixWithWorkspace(workspace, modelID, session string) ([]string, bool) {
	if t.CLI.Launch.ForkExec == "" || !strings.Contains(t.CLI.Launch.ForkExec, PromptToken) {
		return nil, false
	}
	argv := t.ForkArgvWithWorkspace(workspace, modelID, session, PromptToken)
	if len(argv) < 2 || argv[len(argv)-1] != PromptToken {
		return nil, false
	}
	return argv[1 : len(argv)-1], true
}

// Model is an inference backend.
type Model struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"canonical model name"` // the alias clients pass
	Aliases         []string `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"alternate accepted names"`
	Display         string   `yaml:"display,omitempty" json:"display,omitempty" doc:"human-facing label"`
	// Kind is HOW YOU AUTHENTICATE: subscription | api | local.
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty" doc:"authentication mode"`

	// Billing is HOW YOU PAY: metered | flat | free. Optional — when empty it is
	// derived from Kind by BillingMode(), reproducing the old collapsed behaviour, so
	// no existing model needs touching.
	//
	// It exists because z.ai's GLM Coding Plan is flat-rate billing over an API key,
	// and no single value of Kind can say that. See the constants above.
	Billing string `yaml:"billing,omitempty" json:"billing,omitempty" doc:"billing mode"`

	Source string `yaml:"source,omitempty" json:"source,omitempty" doc:"model source: cloud or local"`

	Provider  string `yaml:"provider,omitempty" json:"provider,omitempty" doc:"inference provider"`
	BaseURL   string `yaml:"base_url,omitempty" json:"base_url,omitempty" doc:"provider API base URL"`
	APIKeyRef string `yaml:"api_key_ref,omitempty" json:"api_key_ref,omitempty" doc:"credential-store key name"`
	// UpstreamID is the provider-side model id — the value handed to a
	// tool's --model flag. Its YAML key is `model:`, matching the asset
	// registry's column.
	UpstreamID string `yaml:"model,omitempty" json:"model,omitempty" doc:"default provider-side model id"`

	// ToolIDs override UpstreamID for a specific tool, because THE ID A MODEL
	// ANSWERS TO IS A PROPERTY OF THE TOOL, NOT OF THE MODEL.
	//
	// One model, three spellings, all live today:
	//
	//   aider/opencode  deepseek/deepseek-v4-pro   (litellm wants provider/model)
	//   ycode           deepseek-v4-pro            (it detects the provider itself)
	//   agy             Gemini 3.1 Pro (High)      (a display string, not a slug)
	//
	// Treating UpstreamID as one global value made ycode's bindings dead on
	// arrival: the registry handed it litellm's prefixed form and ycode rejected
	// it, while the same model worked perfectly when ycode was run by hand. That
	// is the whole dead-binding failure mode again, and `agents verify --live`
	// caught it within a minute of the tool being registered.
	//
	// Keyed by TOOL name. Absent → UpstreamID.
	ToolIDs map[string]string `yaml:"ids,omitempty" json:"ids,omitempty" doc:"tool-specific provider-side model ids"`

	// Family and Version make the canonical name version-explicit. The
	// catalog derives the floating family alias from them: `opus` names
	// whichever member of family `opus` has the highest Version. A record
	// therefore stores `claude:opus5`, which is true forever, while the
	// convenient `opus` re-points on its own when a release lands.
	//
	// Family is declared, never parsed out of the name: `kimi-k2.7-code`
	// and `kimi-k2.6` are separate product lines, and no amount of clever
	// suffix-stripping gets that right.
	Family  string `yaml:"family,omitempty" json:"family,omitempty" doc:"version-independent product family"`
	Version string `yaml:"version,omitempty" json:"version,omitempty" doc:"version within the product family"`

	// Band is the model's capability peg, 1 (basic) to MaxBand (frontier); 0 is
	// unpegged. It is normalized ACROSS providers — a provider's own tier
	// ladder is never mapped positionally, so four vendor tiers may all
	// land in one band. Agents inherit it; they never carry their own.
	Band int `yaml:"band,omitempty" json:"band,omitempty" doc:"normalized capability band"`

	// BandSource says whether the band was MEASURED or merely DECLARED, and it
	// exists because the fleet has already been burned once by not knowing.
	//
	// "declared" is a considered guess from provider tier + priors. "measured"
	// means the model was run up a difficulty ladder and pegged at the highest
	// rung it reliably cleared — which is the only thing a band actually means.
	//
	// The distinction is load-bearing: a quiz cannot validate a band. Every agent
	// in this fleet scores 5/5 on L1-difficulty questions, so passing an easy test
	// is evidence of nothing. A band is the highest rung you CLEAR, not a score,
	// and until a model has failed something it has not been placed.
	//
	// Empty means declared. Nothing should present an unmeasured band as fact.
	BandSource string `yaml:"band_source,omitempty" json:"band_source,omitempty" doc:"evidence supporting the capability band"`

	// BandSeason is the season the peg was set. Under the band ladder a peg is
	// only a SEED for the agents bound to this model, and it expires
	// SeatSeasons after it was set unless their gates confirm it. A peg with
	// no season is unclocked: it is reported as such, never silently aged.
	BandSeason int `yaml:"band_season,omitempty" json:"band_season,omitempty" doc:"season the capability band peg was set"`

	// Tier is the provider's own word for its tier, carried from an org
	// overlay. It is not Band and is not routable.
	Tier string `yaml:"tier,omitempty" json:"tier,omitempty" doc:"provider-native tier name"`

	// Plan names the subscription plan (a record under plans/) this model is
	// PAID THROUGH — the seat, not the vendor of the weights: a Claude model
	// served by agy draws on the Google plan, not the Anthropic one. Empty or
	// dangling means unknown, and unknown is fail-open everywhere it is read
	// (see Plan and PlanTierRank).
	Plan          string   `yaml:"plan,omitempty" json:"plan,omitempty" doc:"subscription plan id the model is billed through (plans/)"`
	Capabilities  []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty" doc:"declared model capabilities"`
	Domain        []string `yaml:"domain,omitempty" json:"domain,omitempty" doc:"preferred task domains"`
	ContextLength int64    `yaml:"context_length,omitempty" json:"context_length,omitempty" doc:"maximum context length"`
	Price         float64  `yaml:"price,omitempty" json:"price,omitempty" doc:"relative provider price"`

	// Quality is the model's overall capability prior in [0,1]; Spec holds
	// per-capability adjustments where a model is notably stronger or
	// weaker than its tier. CostMicro is the relative per-turn cost the
	// routing objective divides by. All three are read by the capability
	// matrix.
	Quality   float64            `yaml:"quality,omitempty" json:"quality,omitempty" doc:"overall capability prior"`
	CostMicro int64              `yaml:"cost_micro,omitempty" json:"cost_micro,omitempty" doc:"relative per-turn cost"`
	Spec      map[string]float64 `yaml:"spec,omitempty" json:"spec,omitempty" doc:"per-capability adjustments"`

	XHosts []ModelHost `yaml:"x_hosts,omitempty" json:"x_hosts,omitempty" doc:"paired hosts serving the model"`

	// Derived holds names the catalog computed at load — today, the family
	// alias. It is a function of the whole catalog, not of this entry, so
	// it is never persisted (`yaml:"-"`): writing it back would freeze a
	// pointer that is supposed to float.
	Derived []string `yaml:"-" json:"derived,omitempty"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// ModelHost names a paired host serving a projected local model.
type ModelHost struct {
	Host  string `yaml:"host" json:"host" doc:"paired host name"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty" doc:"host owner"`
}

// Target is the id passed to a tool's model flag: the provider-side id
// when known, else the alias itself.
//
// Prefer TargetFor: the id a model answers to depends on WHICH TOOL is asking.
func (m Model) Target() string {
	if m.UpstreamID != "" {
		return m.UpstreamID
	}
	return m.Name
}

// TargetFor is the id THIS TOOL will accept for this model.
//
// The same model is spelled differently by different harnesses — litellm wants
// `deepseek/deepseek-v4-pro`, ycode wants `deepseek-v4-pro`, agy wants
// `Gemini 3.1 Pro (High)`. A registry that stores one global id hands the wrong
// string to somebody, and a wrong model id is a DEAD BINDING: it looks perfectly
// healthy until an agent tries to speak.
func (m Model) TargetFor(tool string) string {
	if id, ok := m.ToolIDs[tool]; ok && id != "" {
		return id
	}
	return m.Target()
}

// AgentFile is the on-disk envelope for agents. It mirrors the asset
// registry's shape, where one file may declare several agents.
type AgentFile struct {
	New      bool    `yaml:"new,omitempty" json:"new,omitempty"`
	LogLevel string  `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	Agents   []Agent `yaml:"agents" json:"agents"`
}

// Agent is a tool bound to a model, under a nickname.
type Agent struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"canonical agent name"` // the primary nickname
	Aliases         []string `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"alternate accepted names"`
	Display         string   `yaml:"display,omitempty" json:"display,omitempty" doc:"human-facing label"`
	Description     string   `yaml:"description,omitempty" json:"description,omitempty" doc:"purpose of the agent"`

	// Nick is the agent's human name — the one you say out loud. Leave it
	// empty and the catalog assigns one deterministically from the binding,
	// so every agent has a memorable handle without anyone naming it.
	Nick string `yaml:"nick,omitempty" json:"nick,omitempty" doc:"human name used in conversation"`

	Tool  string `yaml:"tool" json:"tool" doc:"bound tool name"`    // → Tool.Name
	Model string `yaml:"model" json:"model" doc:"bound model name"` // → Model.Name

	// Effort is the reasoning-effort level this binding is DECLARED to run at
	// (e.g. low, medium, high). It is a property of the binding, not of the
	// model (the same model may run at different efforts under different
	// tools) nor of the tool (one tool serves many models). Empty = the tool's
	// own default, untouched. The model door (cligw) passes it to the CLI —
	// claude `--effort L`, codex `-c model_reasoning_effort="L"` — refuses a
	// tool with no known effort flag, and freezes it into the sticky identity.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty" doc:"declared reasoning effort (low, medium, high, …); empty = the tool's default"`

	// A CASCADE agent (band_source: cascade) is not a plain tool:model binding.
	// It runs a cheap Base agent and, when the base gets stuck, escalates through
	// Escalation (a ladder of agent names, tried in order — e.g. an L3 then an L4)
	// for a content-full steer. It SERVES at Band via that ladder while running
	// cheap most of the time. When Base is set, Model is ignored.
	Base       string   `yaml:"base,omitempty" json:"base,omitempty" doc:"base agent for a cascade"`
	Escalation []string `yaml:"escalation,omitempty" json:"escalation,omitempty" doc:"cascade escalation ladder"`

	// Band + BandSource are the SERVED band of a cascade agent (BandSource
	// "cascade") — the level the ladder REACHES, not the base model's peg. This
	// is the one legitimate agent-level band: it is the cascade's contract, not a
	// stored model peg that would rot. For a plain tool:model agent these are
	// empty and the band is inherited from the model, as always.
	Band       int    `yaml:"band,omitempty" json:"band,omitempty" doc:"served capability band for a cascade"`
	BandSource string `yaml:"band_source,omitempty" json:"band_source,omitempty" doc:"source of the served band"`

	// Ratings, Certificates and Seat are the band ladder's EVIDENCE (see
	// ladder.go). The band itself is derived from them and never stored: an
	// agent holds band n only while every gate G1..Gn holds.
	Ratings      *DutyRatings  `yaml:"ratings,omitempty" json:"ratings,omitempty" doc:"duty ratings (code, manage, judge)"`
	Certificates []Certificate `yaml:"certificates,omitempty" json:"certificates,omitempty" doc:"passed certification suites, bound to model and season"`
	Seat         *Seat         `yaml:"seat,omitempty" json:"seat,omitempty" doc:"operator or provisional band seat; a seed that expires"`

	Role        *AgentRole        `yaml:"role,omitempty" json:"role,omitempty" doc:"permissions and scope"`
	Ledger      *AgentLedger      `yaml:"ledger,omitempty" json:"ledger,omitempty" doc:"operational reliability record"`
	Instruction *AgentInstruction `yaml:"instruction,omitempty" json:"instruction,omitempty" doc:"standing instruction"`
	Functions   []string          `yaml:"functions,omitempty" json:"functions,omitempty" doc:"available function kits"`

	// ClonedFrom and ClonedAt record that this agent was BRANCHED off another,
	// and when.
	//
	// An agent is a singleton identity — one conversation store, one kb
	// attribution, one bus cursor — so two concurrent tasks cannot be given to
	// one agent without mixing their context, and mixed context produces
	// confidently wrong answers. Parallelism is therefore expressed as MORE
	// AGENTS, and a clone is how you get one that starts from somewhere rather
	// than from nothing: it inherits its parent's context as of ClonedAt and
	// diverges from that moment on.
	//
	// The provenance is kept because the alternative is a fleet of same-binding
	// agents with no way to tell which was the original, which was branched off
	// what, or when their histories parted.
	ClonedFrom string `yaml:"cloned_from,omitempty" json:"cloned_from,omitempty" doc:"parent agent name"`
	ClonedAt   string `yaml:"cloned_at,omitempty" json:"cloned_at,omitempty" doc:"clone creation time"`

	// Ephemeral marks a clone minted for ONE task, to be removed when that task
	// closes. It is hidden from `agents list` unless --all, because a fleet
	// roster listing every in-flight task's worker is a roster nobody reads.
	// Task, when set, names the work it was minted for.
	Lifecycle *AgentLifecycle `yaml:"lifecycle,omitempty" json:"lifecycle,omitempty" doc:"work owning this ephemeral definition"`
	Ephemeral bool            `yaml:"ephemeral,omitempty" json:"ephemeral,omitempty" doc:"whether the agent exists for one task"`
	Task      string          `yaml:"task,omitempty" json:"task,omitempty" doc:"task assigned to an ephemeral agent"`

	// AutoNick and Derived are computed by the catalog at load: the
	// assigned human name (when Nick is empty) and the floating family
	// alias (`claude-opus` for a binding on `opus5`). Both are functions
	// of the whole catalog, so neither is ever persisted.
	// Unavailable is derived from retired dependencies; it never retires this identity.
	Unavailable string   `yaml:"-" json:"unavailable,omitempty"`
	AutoNick    string   `yaml:"-" json:"auto_nick,omitempty"`
	Derived     []string `yaml:"-" json:"derived,omitempty"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// IsCascade reports whether this agent is a composite cascade (a cheap Base that
// escalates through a ladder), as opposed to a plain tool:model binding.
func (a *Agent) IsCascade() bool {
	return a.BandSource == "cascade" && a.Base != ""
}

type AgentRole struct {
	Skills       []string `yaml:"skills,omitempty" json:"skills,omitempty" doc:"skills assigned to the role"`
	AllowedTools []string `yaml:"allowed_tools,omitempty" json:"allowed_tools,omitempty" doc:"tools permitted for the role"`
	Scope        string   `yaml:"scope,omitempty" json:"scope,omitempty" doc:"role responsibility boundary"`
}

type AgentLedger struct {
	Reliability string `yaml:"reliability,omitempty" json:"reliability,omitempty" doc:"operability prior"`
	Notes       string `yaml:"notes,omitempty" json:"notes,omitempty" doc:"reliability notes"`
}

type AgentInstruction struct {
	Content string `yaml:"content,omitempty" json:"content,omitempty" doc:"instruction text"`
}

// MatrixKey is the agent's identity: tool:model. Every nickname for the
// same binding yields the same key, which is why aliasing never
// fragments the capability matrix.
func (a Agent) MatrixKey() string { return a.Tool + ":" + a.Model }

// Person is a human principal. Standalone-first: a local entry needs no
// account. When the host is paired, Email is the authoritative identity.
type Person struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Handle          string   `yaml:"handle" json:"handle"`
	Aliases         []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Display         string   `yaml:"display,omitempty" json:"display,omitempty"`
	Email           string   `yaml:"email,omitempty" json:"email,omitempty"`

	// OSUsers maps a host name to this person's account name there. It is
	// deliberately per-host: assuming the local $USER exists on a remote
	// box is the single most common way a cross-host reach fails.
	OSUsers map[string]string `yaml:"os_users,omitempty" json:"os_users,omitempty"`
	// DefaultOSUser is used for hosts absent from OSUsers.
	DefaultOSUser string `yaml:"default_os_user,omitempty" json:"default_os_user,omitempty"`

	Hosts  []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
	Source string   `yaml:"source,omitempty" json:"source,omitempty"` // local | cloud

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// OSUserFor returns this person's account name on host, and whether the
// binding was explicit. A false second result means the caller is about
// to guess — say so rather than silently assuming.
func (p Person) OSUserFor(host string) (string, bool) {
	if u, ok := p.OSUsers[host]; ok && u != "" {
		return u, true
	}
	if p.DefaultOSUser != "" {
		return p.DefaultOSUser, true
	}
	return "", false
}

// names returns an entry's canonical name followed by its aliases.
func names(name string, aliases []string) []string {
	out := make([]string, 0, len(aliases)+1)
	if name != "" {
		out = append(out, name)
	}
	seen := map[string]bool{name: true}
	for _, a := range aliases {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Names returns the tool's canonical name and every alias.
func (t Tool) Names() []string { return names(t.Name, t.Aliases) }

// Names returns the model's canonical name and every alias it answers to,
// including the catalog-derived family alias.
func (m Model) Names() []string {
	return names(m.Name, append(append([]string{}, m.Aliases...), m.Derived...))
}

// Names returns the agent's canonical nickname and every alias it answers
// to: declared aliases, its human name, and the catalog-derived family
// alias. One list, so every resolver — whois, chat, meet, weave — sees the
// same set of names without knowing which were declared and which derived.
func (a Agent) Names() []string {
	extra := append([]string{}, a.Aliases...)
	if n := a.NickName(); n != "" {
		extra = append(extra, n)
	}
	return names(a.Name, append(extra, a.Derived...))
}

// NickName is the agent's human name: the one it was given, else the one
// the catalog assigned it.
func (a Agent) NickName() string {
	if a.Nick != "" {
		return a.Nick
	}
	return a.AutoNick
}

// Names returns the person's handle and every alias.
func (p Person) Names() []string { return names(p.Handle, p.Aliases) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EventsArgv renders the tool's event-channel flag for a given path, or nil when
// the tool cannot stream events (which is every third-party CLI we have).
func (t Tool) HasEventsArg() bool { return strings.TrimSpace(t.CLI.Launch.EventsArg) != "" }

// EventsStdoutArgv renders the fixed argv that puts a tool's event stream on
// stdout. No {path}: these tools take no path, which is the whole difference.
func (t Tool) EventsStdoutArgv() []string {
	f := strings.Fields(strings.TrimSpace(t.CLI.Launch.EventsStdout))
	if len(f) == 0 {
		return nil
	}
	return f
}

func (t Tool) EventsArgv(path string) []string {
	tmpl := strings.TrimSpace(t.CLI.Launch.EventsArg)
	if tmpl == "" || strings.TrimSpace(path) == "" {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(tmpl) {
		out = append(out, strings.ReplaceAll(f, "{path}", path))
	}
	return out
}

// ReportsTurnEnd says whether this tool tells us when a turn is over, instead of
// leaving us to infer it from silence.
//
// EITHER ROUTE COUNTS. This used to read EventsArg alone, which was true when
// ycode's side-channel file was the only way a tool could speak — and became
// wrong the moment a stdout streamer was declarable. Left as it was, claude,
// codex and agy would each stream a perfectly good turn boundary while this
// reported them silent, and every caller would keep paying the 25-second tax to
// re-derive a fact already on the wire.
func (t Tool) ReportsTurnEnd() bool {
	return t.StreamsEvents() && t.CLI.Launch.EventsDone.Declared() || strings.TrimSpace(t.CLI.Launch.EventsArg) != ""
}

// BillingMode returns how this model is paid for, deriving it from Kind when the
// Billing field is absent.
//
// The derivation reproduces exactly what the collapsed enum used to mean, which is what
// makes this a purely additive change: a model written before `billing:` existed keeps
// its old semantics with no edit.
//
//	subscription -> flat      (a seat you already paid for)
//	api          -> metered   (you pay per token)
//	local        -> free      (your own hardware)
func (m Model) BillingMode() string {
	switch m.Billing {
	case BillingMetered, BillingFlat, BillingFree, BillingFlatThenMetered:
		return m.Billing
	}
	switch m.Kind {
	case ModelKindSubscription:
		// A vendor seat overruns into pay-as-you-go rather than blocking — that is how
		// Anthropic Max/Pro and Codex behave, and they are every subscription we have.
		// Defaulting to the SILENT-COST mode rather than the loud one is deliberate: if
		// the guess is wrong the operator is warned about a bill that cannot arrive,
		// which is a harmless false alarm. The other way round, the warning is missing
		// exactly when the money is moving.
		return BillingFlatThenMetered
	case ModelKindLocal:
		return BillingFree
	case ModelKindAPI:
		return BillingMetered
	}
	return ""
}

// OverrunsIntoMoney reports whether exhausting this model's quota starts BILLING rather
// than blocking. The one thing an unattended run needs to know before it starts.
func (m Model) OverrunsIntoMoney() bool {
	return m.BillingMode() == BillingFlatThenMetered
}

// MarginalCostMicro is the cost of the NEXT token — the only cost a routing decision
// can actually act on, and not always CostMicro.
//
// Under a FLAT plan no invoice moves when you use it, so the naive reading is "free at
// the margin, prefer it over everything". THAT IS WRONG, and a test caught it: pricing
// every flat plan at a constant floor made a premium Opus/Codex SEAT marginally cheaper
// than metered DeepSeek, so the router would have sent every trivial task to the most
// expensive model in the fleet — inverting the whole point of the band ladder
// ("don't send a premium model to add a line of YAML").
//
// The thing a flat plan is short of is QUOTA, and quota scarcity SCALES WITH THE MODEL.
// A premium seat's quota is precious; a commodity seat's is not. Burning Opus quota on
// a YAML edit is expensive even though no invoice moves.
//
// So a flat plan is a DISCOUNT ON ITS OWN LIST PRICE, never a flat floor:
//
//	metered  -> CostMicro                      (you pay per token)
//	flat     -> CostMicro * FlatPlanDiscount   (prepaid, but the quota is finite)
//	free     -> 0                              (your own hardware)
//
// That keeps both truths at once: a flat model beats a METERED PEER of the same class
// (using capacity you already bought is not a saving to forgo), while a premium seat
// still costs more than a commodity one (its quota is worth more).
func (m Model) MarginalCostMicro() int64 {
	switch m.BillingMode() {
	case BillingFree:
		return 0
	case BillingFlat, BillingFlatThenMetered:
		// Below quota these price identically — the seat is bought either way. They
		// differ in what happens when it runs out (blocked vs billed), which is a
		// failure mode, not a price. See the billing constants.
		c := m.CostMicro * FlatPlanDiscountNum / FlatPlanDiscountDen
		if c < 1 && m.CostMicro > 0 {
			c = 1 // a priced model never becomes literally free
		}
		return c
	default:
		return m.CostMicro
	}
}

// FlatPlanDiscount — what a prepaid seat is worth at the margin, as a fraction of its
// list price. Half: real, but nowhere near free, because the quota is finite.
const (
	FlatPlanDiscountNum = 1
	FlatPlanDiscountDen = 2
)
