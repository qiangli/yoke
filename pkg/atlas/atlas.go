// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package atlas is the Command Atlas: the curated multi-axis catalog of the
// bashy/coreutils command surface. Beyond the classical class split
// (builtin / coreutils / verb) it records, per command, a functional group,
// the dhnt execution tier it operates in (userland/workspace/sandbox/sphere/
// cluster/cloud/account), and agentic capability flags (json, dry-run,
// destructive, …), plus a curated list of composite idioms (commands
// naturally used together).
//
// The atlas is an execution-assist substrate, not just presentation: it is
// imported by `bashy commands` (the views), the MCP server (list_tools
// metadata), and — per the roadmap — pkg/dag target preflight and the
// advisor. It is stdlib-only.
//
// Discipline: every assignment is a hand-set table entry; vocabularies are
// closed; coverage tests in this package and in bashy assert the tables stay
// exactly in sync with the live registries (tool.Names(), the shim lists).
// Shell builtins are NOT recorded here — the embedding shell owns that set
// and merges it in (bashy/internal/agentos/atlas.go). Declarative-registry
// CLIs (doctl, …) are also not hand-listed: their entries derive from
// external/registry's Entry.Tier via RegistryEntry.
//
// Design doc: bashy/docs/command-atlas.md.
package atlas

import (
	"fmt"
	"sort"
	"strings"
)

// Execution tiers (locked vocabulary — dhnt docs/execution-tiers.md), plus
// "account" for the Tessaro front door beside the stack.
const (
	TierUserland  = "userland"
	TierWorkspace = "workspace"
	TierSandbox   = "sandbox"
	TierSphere    = "sphere"
	TierCluster   = "cluster"
	TierCloud     = "cloud"
	TierAccount   = "account"
)

// Functional groups (closed vocabulary). "shell" is reserved for the
// builtins, which the embedding shell contributes.
const (
	GroupShell        = "shell"
	GroupFileutils    = "fileutils"
	GroupTextutils    = "textutils"
	GroupShellutils   = "shellutils"
	GroupCodeIntel    = "code-intel"
	GroupNet          = "net"
	GroupOrch         = "orchestration"
	GroupKnowledge    = "knowledge"
	GroupEngines      = "engines"
	GroupForge        = "forge"
	GroupToolchains   = "toolchains"
	GroupStorage      = "storage"
	GroupClusterCloud = "cluster-cloud"
	GroupPlatform     = "platform"
	GroupDiagnostics  = "diagnostics"
	GroupAccount      = "account"
)

// SDLC stages (closed vocabulary). The spine every front-door verb must place
// itself on: plan → code → test → deploy, plus "cross" for the verbs that serve
// every stage (knowledge, identity, diagnostics, the userland itself).
//
// This axis exists to answer ONE question, asked of every new verb before it
// ships: *which stage do you serve that nothing else already does?* bashy's
// agentic surface grew piecemeal until the Code stage had six overlapping verbs
// and the Test stage had none — a hole nobody could see because there was no
// axis on which to see it. A stage is therefore MANDATORY for a verb: addVerb
// panics without one, so a verb that cannot answer the question cannot start the
// binary. That is deliberately harsher than a test: a test can be defaulted
// around (this one was — see the git history of bashy's verbAtlasRecord, which
// invented a valid-looking group/tier for unclassified verbs and so silently
// defeated the very coverage test that was meant to catch them).
const (
	StagePlan   = "plan"   // decide what to build: sprint, meet, kb
	StageCode   = "code"   // build it: weave, chat
	StageTest   = "test"   // decide pass/fail: dag, check, verify
	StageDeploy = "deploy" // ship it: sdlc, cluster/cloud CLIs
	StageCross  = "cross"  // serves every stage: skills, secrets, doctor, the userland
)

// Output shapes describe what a successful command invocation means to an
// output reducer. A verdict reports a decision (and may be silent on success);
// a result reports the answer in its output, so its bytes must be retained.
type OutputShape string

const (
	ShapeVerdict = "verdict"
	ShapeResult  = "result"
)

// Agentic capability flags (closed vocabulary; curated, never inferred —
// absence means unknown, not no).
const (
	CapJSON             = "json"              // structured-output mode (--json or native)
	CapDryRun           = "dry-run"           // participates in the dry-run manifest
	CapDestructive      = "destructive"       // can irreversibly delete/overwrite data
	CapReadOnly         = "read-only"         // never mutates the filesystem
	CapCached           = "cached"            // keeps a persistent on-disk cache
	CapBudget           = "budget"            // token-budget-aware output
	CapNeedsNetwork     = "needs-network"     // requires network beyond first provision
	CapNeedsPairing     = "needs-pairing"     // requires a Tessaro-paired machine/token
	CapSelfProvisioning = "self-provisioning" // download → verify → cache → exec
	CapSpawnsProcesses  = "spawns-processes"  // executes external processes
	CapDaemon           = "daemon"            // starts/manages a long-running service
)

// Security effects (closed vocabulary; curated, never inferred). Unlike the
// capability flags — which describe what a command is FOR — effects describe
// what a command can DO to the machine, the data, or the outside world, from a
// security / privacy / governance lens. Every atlas entry declares at least one
// (EffPure is the explicit "no governed effect" declaration), so classification
// is mandatory: a command with no declared effect fails the coverage ratchet,
// it does not fail open.
//
// The first six mirror the dhnt skill-CNL effect lattice
// (coreutils/pkg/skills → github.com/dhnt/dhnt/skills); the last five are the
// finer distinctions a shell that an agent drives needs to reason about. A
// future policy engine projects this 11-atom set onto the dhnt 6 for skill-cap
// compatibility.
const (
	EffPure    = "pure"    // deterministic, no governed side effect (true, echo, seq)
	EffRead    = "read"    // reads filesystem / host state / input data (privacy surface)
	EffWrite   = "write"   // mutates the filesystem or host state
	EffDestroy = "destroy" // can IRREVERSIBLY lose data (rm, dd, shred)
	EffNet     = "net"     // opens a network connection (egress / exfiltration surface)
	EffExec    = "exec"    // spawns an external process that bashy no longer governs
	EffCred    = "cred"    // reads or writes credentials / secrets
	EffPriv    = "priv"    // changes privilege, ownership, or a security label
	EffRemote  = "remote"  // executes on ANOTHER host (crosses the machine boundary)
	EffPersist = "persist" // leaves something that OUTLIVES the session (cron, daemon, install)
	EffSpend   = "spend"   // incurs metered cost (paid inference, cloud resources)
)

// Subclass refines the verb class only.
const (
	SubclassProvisioner     = "provisioner"
	SubclassManagedExternal = "managed-external"
	SubclassRegistered      = "registered" // an operator-registered command (registered.go)
)

// Origin says WHERE a command came from — the provenance axis. It is
// exclusive: one origin per command. The classical class (builtin / coreutils
// / verb) says how a name RESOLVES; the origin says who defined it. The two
// disagree in useful ways: `printf` resolves as a bash builtin but is a GNU
// coreutils command; `awk` is in-process Go but nobody at GNU wrote it.
//
// POSIX is deliberately NOT an origin: the 116 POSIX-required names cut
// across bash (`cd`), GNU (`cat`), classic Unix (`awk`) and the pinned
// external providers (`m4`), so a flat enum would have to pick between
// "GNU" and "POSIX" for `cat` and every reader would ask which won. It is
// the `Posix` tag instead (PosixRequired()).
const (
	OriginBash     = "bash"     // bash 5.3 builtin, contributed by the embedding shell
	OriginGNU      = "gnu"      // GNU coreutils command reimplemented in Go
	OriginUnix     = "unix"     // other classic Unix tool reimplemented in Go (awk, sed, jq, tar, …)
	OriginExternal = "external" // bin-managed: binmgr CLI, toolchain provisioner, pinned POSIX provider — exec'd, never linked
	OriginBashy    = "bashy"    // the YOKE commands: bashy's agentic third substrate (Classic · Bash++ · Yoke) — built for agentic tools; many need no model (deterministic rungs: tz, clip, tokens)
	// OriginRegistered marks a command the OPERATOR added with `bashy commands
	// add` (or an org published through a shared ring) — the one origin that is
	// never a table entry. Its atlas record is derived from the record's data
	// by RegisteredEntry, the same way a declarative-registry CLI's is by
	// RegistryEntry. See registered.go.
	OriginRegistered = "registered"
)

// Entry is one command's atlas record. The classical class (builtin /
// coreutils / verb) is not stored: it follows from which table (or the
// embedding shell's builtin set) the name resolves in.
type Entry struct {
	Group    string
	Tier     string
	Stage    string      // SDLC stage (closed vocab); every VERB declares one
	Shape    OutputShape // output shape: verdict | result; unknown values are result
	Subclass string      // verbs only: provisioner | managed-external | ""
	Caps     []string
	Effects  []string // security effects (closed vocab); every entry has ≥1
	AliasOf  string   // e.g. docker → podman, upgrade → self
	Origin   string   // provenance (closed vocab, exclusive); every entry has one
	Posix    bool     // one of the 116 POSIX-required names (cross-cuts Origin)
	OS       []string // platforms the command is supported on (closed vocab; platform.go)
	Partial  []string // supported platforms where it runs with a documented gap

	// Web declares a browser UI, and is how `bashy web-console` discovers what
	// to put on the start page without a hardcoded table. Nil = no web surface.
	// See web.go.
	Web *WebSurface
}

// Shapes returns the closed output-shape vocabulary.
func Shapes() []OutputShape { return []OutputShape{ShapeResult, ShapeVerdict} }

// NormalizeShape returns the safe reducer interpretation of shape. Unknown
// and empty values deliberately remain result-shaped: under-compressing is
// visible, while discarding a command's answer is not.
func NormalizeShape(shape OutputShape) OutputShape {
	if shape == ShapeVerdict {
		return ShapeVerdict
	}
	return ShapeResult
}

// OutputShape returns the reducer shape for an entry, applying the
// conservative result default to unclassified or future values.
func (e Entry) OutputShape() OutputShape { return NormalizeShape(e.Shape) }

// verdictSubcommands is intentionally small and positive-only. Atlas entries
// are keyed by argv[0], but a compiler or VCS front door can have commands
// whose output is either the answer or merely a pass/fail verdict. Anything
// absent from this table remains result-shaped.
var verdictSubcommands = map[string]map[string]OutputShape{
	"go":    {"build": ShapeVerdict, "test": ShapeVerdict, "vet": ShapeVerdict},
	"git":   {"push": ShapeVerdict},
	"cargo": {"build": ShapeVerdict, "test": ShapeVerdict},
	"npm":   {"test": ShapeVerdict},
}

// ResolveOutputShape returns the reducer shape for an argv. A known external
// subcommand receives its curated shape; unknown programs and subcommands
// fail closed to result so their output is never discarded. This supplements,
// rather than replaces, the argv[0]-keyed Entry shape.
func ResolveOutputShape(argv []string) OutputShape {
	if len(argv) == 0 {
		return ShapeResult
	}
	if subcommands, ok := verdictSubcommands[argv[0]]; ok {
		if len(argv) > 1 {
			if shape, ok := subcommands[argv[1]]; ok {
				return shape
			}
		}
		return ShapeResult
	}
	if entry, ok := Lookup(argv[0]); ok {
		return entry.OutputShape()
	}
	return ShapeResult
}

// Idiom is one curated composite: commands naturally used together.
type Idiom struct {
	ID       string   `json:"id"`
	Commands []string `json:"commands"`
	Pattern  string   `json:"pattern"`
	Note     string   `json:"note"`
	Fused    string   `json:"fused,omitempty"` // shipped fused form, if any
	Tier     string   `json:"tier"`
}

var (
	tools = map[string]Entry{}
	verbs = map[string]Entry{}
)

// Groups returns the closed group vocabulary, sorted.
func Groups() []string {
	return []string{
		GroupAccount, GroupClusterCloud, GroupCodeIntel, GroupDiagnostics,
		GroupEngines, GroupFileutils, GroupForge, GroupKnowledge, GroupNet,
		GroupOrch, GroupPlatform, GroupShell, GroupShellutils, GroupStorage,
		GroupTextutils, GroupToolchains,
	}
}

// Tiers returns the tier vocabulary in stack order (foundation → payoff),
// with account last (beside the stack, not in it).
func Tiers() []string {
	return []string{
		TierUserland, TierWorkspace, TierSandbox, TierSphere,
		TierCluster, TierCloud, TierAccount,
	}
}

// Capabilities returns the closed capability vocabulary, sorted.
func Capabilities() []string {
	return []string{
		CapBudget, CapCached, CapDaemon, CapDestructive, CapDryRun,
		CapJSON, CapNeedsNetwork, CapNeedsPairing, CapReadOnly,
		CapSelfProvisioning, CapSpawnsProcesses,
	}
}

// Stages returns the closed SDLC-stage vocabulary, in pipeline order (not
// sorted: the order IS the spine, and reading it out of order loses the point).
func Stages() []string {
	return []string{StagePlan, StageCode, StageTest, StageDeploy, StageCross}
}

// Effects returns the closed security-effect vocabulary, sorted.
func Effects() []string {
	return []string{
		EffCred, EffDestroy, EffExec, EffNet, EffPersist, EffPriv,
		EffPure, EffRead, EffRemote, EffSpend, EffWrite,
	}
}

// Origins returns the closed origin vocabulary in presentation order:
// the shell first, then the userland by how far it is from the standard,
// then what bashy exec's, then what bashy invented.
func Origins() []string {
	return []string{OriginBash, OriginGNU, OriginUnix, OriginExternal, OriginBashy, OriginRegistered}
}

// OriginLabel is the human/agent-readable name of an origin, for listings.
// The bashy-added group is called the YOKE commands — one proper noun beside
// bash · GNU · Unix · external, so the group can be referred to as easily as
// "the GNU coreutils" or "the POSIX-required set". `commands` minus yoke is
// the classic surface. Agentic means BUILT FOR AGENTIC TOOLS, not "needs a
// model": the ladder has deterministic rungs (tz, clip, duration, tokens)
// that are yoke all the same. The wire value stays
// "bashy" (provenance = who), the label says yoke.
func OriginLabel(origin string) string {
	switch origin {
	case OriginBash:
		return "bash builtin"
	case OriginGNU:
		return "GNU coreutils"
	case OriginUnix:
		return "classic Unix"
	case OriginExternal:
		return "bin-managed external"
	case OriginBashy:
		return "yoke — added by bashy"
	case OriginRegistered:
		return "registered — added with bashy commands add"
	}
	return origin
}

// GNUCoreutilsUpstream returns the GNU coreutils 9.x command inventory
// (108 names), sorted. It is the membership test behind OriginGNU and the
// upstream side of `bashy commands --gnu`. Three of them (chroot, coreutils,
// runcon) have no bashy implementation and so appear in no table.
func GNUCoreutilsUpstream() []string {
	return append([]string(nil), gnuCoreutilsUpstream...)
}

// PosixRequired returns the 116 POSIX-required utility names — the set
// `posix-gate` certifies (docs/posix-required-commands.tsv), sorted. Names
// owned by the shell (`cd`, `alias`, …) are in this list but in no atlas
// table; the embedder tags its builtins from it.
func PosixRequired() []string {
	return append([]string(nil), posixRequired...)
}

// IsPosixRequired reports whether name is one of the 116.
func IsPosixRequired(name string) bool { return posixRequiredSet[name] }

// Lookup returns the atlas entry for a command name: in-process tools first,
// then front-door verbs (mirroring dispatch precedence). Shell builtins and
// declarative-registry CLIs are the embedder's to merge (see RegistryEntry).
func Lookup(name string) (Entry, bool) {
	if e, ok := tools[name]; ok {
		e.Shape = NormalizeShape(e.Shape)
		return e, true
	}
	e, ok := verbs[name]
	if ok {
		e.Shape = NormalizeShape(e.Shape)
	}
	return e, ok
}

// ToolNames returns the names of the in-process (coreutils-tool-class)
// entries, sorted. Coverage tests assert this set == tool.Names().
func ToolNames() []string { return sortedKeys(tools) }

// VerbNames returns the names of the front-door-verb-class entries, sorted.
// The declarative-registry CLIs are not included (derive via RegistryEntry).
func VerbNames() []string { return sortedKeys(verbs) }

// RegistryEntry returns the derived atlas entry for a declarative-registry
// CLI (external/registry), given its Entry.Tier int. Registry CLIs are never
// hand-listed in the atlas: new providers are registry data only.
func RegistryEntry(tier int) Entry {
	// Every managed external is downloaded (net) and then run as its own process
	// (exec). Only a tier-4+ CLI (sphere/cluster/cloud — doctl, …) drives a
	// control plane on ANOTHER host and so is also `remote`; a tier-2/3 local
	// tool like ripgrep is not.
	effects := []string{EffExec, EffNet}
	if tier >= 4 {
		effects = append(effects, EffRemote)
	}
	sort.Strings(effects)
	// SDLC stage follows the same tier split: a tier-4+ CLI drives a control
	// plane on another host — that is shipping (deploy). A local tier-2/3 tool
	// like ripgrep serves every stage (cross).
	stage := StageCross
	if tier >= 4 {
		stage = StageDeploy
	}
	return Entry{
		Group:    GroupClusterCloud,
		Tier:     TierName(tier),
		Stage:    stage,
		Shape:    ShapeResult,
		Subclass: SubclassManagedExternal,
		Origin:   OriginExternal,
		Posix:    false,
		OS:       []string{OSDarwin, OSLinux, OSWindows},
		Caps: []string{
			CapCached, CapNeedsNetwork, CapSelfProvisioning, CapSpawnsProcesses,
		},
		Effects: effects,
	}
}

// TierName maps the numeric execution tier (external/registry Entry.Tier,
// dhnt docs/execution-tiers.md) to the atlas tier name.
func TierName(t int) string {
	switch t {
	case 2:
		return TierWorkspace
	case 3:
		return TierSandbox
	case 4:
		return TierSphere
	case 5:
		return TierCluster
	case 6:
		return TierCloud
	default:
		return TierUserland
	}
}

// Idioms returns the curated composite list.
func Idioms() []Idiom {
	out := make([]Idiom, len(idioms))
	copy(out, idioms)
	return out
}

// idioms is the curated composite set. Growth rule: additions edit this
// table AND bashy/docs/command-atlas.md together; the coverage test asserts
// every referenced command resolves in the atlas (or is a known builtin).
var idioms = []Idiom{
	{ID: "count-matches", Commands: []string{"grep", "wc"},
		Pattern: "grep PAT F | wc -l", Fused: "grep -c PAT F",
		Note: "one process, one pipe fewer", Tier: TierUserland},
	{ID: "top-n", Commands: []string{"sort", "uniq", "head"},
		Pattern: "... | sort | uniq -c | sort -rn | head",
		Note:    "fusion candidate (bounded-heap top-N verb); no fused form shipped yet",
		Tier:    TierUserland},
	{ID: "find-exec", Commands: []string{"find", "xargs"},
		Pattern: "find ... -print0 | xargs -0 CMD",
		Note:    "the canonical scale-out; -print0/-0 for arbitrary names", Tier: TierUserland},
	{ID: "scoped-cd", Commands: []string{"cd"},
		Pattern: "(cd DIR && CMD)",
		Note:    "subshell keeps the cwd change scoped; avoid bare cd", Tier: TierUserland},
	{ID: "list-inspect", Commands: []string{"ls", "stat"},
		Pattern: "ls DIR; stat FILE",
		Note:    "enumerate, then inspect the interesting entry precisely", Tier: TierUserland},
	{ID: "tempfile-cleanup", Commands: []string{"mktemp", "rm", "trap"},
		Pattern: `t=$(mktemp) && trap 'rm -f "$t"' EXIT`,
		Note:    "leak-free scratch files", Tier: TierUserland},
	{ID: "archive", Commands: []string{"tar"},
		Pattern: "tar -czf out.tgz DIR",
		Note:    "tar+gzip in one call; avoid tar | gzip", Tier: TierUserland},
	{ID: "fetch-extract", Commands: []string{"fetch", "jq"},
		Pattern: "fetch --json URL | jq .field",
		Note:    "HTTP + structured extraction without a browser", Tier: TierUserland},
	{ID: "forge-loop", Commands: []string{"git", "gh", "act"},
		Pattern: "git push; gh pr create; act",
		Note:    "commit/push → PR → run the workflow locally before CI", Tier: TierUserland},
	{ID: "fleet-suite", Commands: []string{"weave", "sprint", "dag"},
		Pattern: "sprint (plan) → weave (isolate/run) → dag (targets)",
		Note: "the orchestration suite (sprint + weave are the public surface; " +
			"foreman/supervise/pair are suppressed internals, Bashy #40)", Tier: TierWorkspace},
	{ID: "cluster-deploy", Commands: []string{"kubectl", "helm"},
		Pattern: "kubectl get ...; helm install ...",
		Note:    "inspect the cluster, install/upgrade via charts", Tier: TierCluster},
	{ID: "pair-first", Commands: []string{"login", "peer"},
		Pattern: "login, then sphere/kubectl",
		Note:    "tiers 4-5 need a Tessaro-paired machine", Tier: TierAccount},
	{ID: "whois-notify", Commands: []string{"whois", "notify"},
		Pattern: "whois AGENT; notify AGENT \"subject\"",
		Note:    "resolve the recipient before sending a durable, attributed nudge", Tier: TierWorkspace},
	{ID: "who-write", Commands: []string{"who", "write"},
		Pattern: "who; write USER",
		Note:    "inspect the live audience before opening a terminal conversation", Tier: TierUserland},
	{ID: "mb-meet", Commands: []string{"mb", "meet"},
		Pattern: "mb; meet --room ROOM",
		Note:    "turn a board handoff into a shared conversation", Tier: TierWorkspace},
	{ID: "notify-inbox", Commands: []string{"notify", "inbox"},
		Pattern: "notify AGENT \"subject\"; inbox",
		Note:    "send a subject-only doorbell and receive it through the same bus", Tier: TierWorkspace},
}

// --- table construction -----------------------------------------------------

func addTools(group string, names ...string) {
	for _, n := range names {
		if _, dup := tools[n]; dup {
			panic(fmt.Sprintf("atlas: duplicate tool entry %q", n))
		}
		// The userland serves every stage — `grep` is not a "test" command any
		// more than it is a "deploy" one. Only front-door VERBS take a position
		// on the spine.
		tools[n] = Entry{Group: group, Tier: TierUserland, Stage: StageCross, Shape: ShapeResult}
	}
}

func addVerb(name string, e Entry) {
	if _, dup := verbs[name]; dup {
		panic(fmt.Sprintf("atlas: duplicate verb entry %q", name))
	}
	if e.Tier == "" {
		e.Tier = TierUserland
	}
	e.Shape = NormalizeShape(e.Shape)
	// A verb MUST place itself on the SDLC spine. This panics at init rather
	// than failing a test, because a test can be defaulted around and this one
	// was: bashy's verbAtlasRecord used to invent a valid-looking group/tier for
	// any verb missing an entry, so the coverage test that was supposed to catch
	// the omission passed happily instead. An unclassifiable verb should not be
	// able to start the binary.
	if !validStage(e.Stage) {
		panic(fmt.Sprintf("atlas: verb %q has no SDLC stage (one of %v). "+
			"Which stage does it serve that nothing else already does? If the honest "+
			"answer is 'none', it should not ship.", name, Stages()))
	}
	// A declared web surface is discovery data the console trusts at runtime, so
	// its invariants are enforced where every other atlas invariant is: at init,
	// loudly. A duplicate mount would silently shadow one surface with another,
	// and a reserved mount would produce a tile that can never be published.
	if e.Web != nil {
		if !validWebMode(e.Web.Mode) {
			panic(fmt.Sprintf("atlas: verb %q declares web mode %q (one of %v)",
				name, e.Web.Mode, WebModes()))
		}
		if e.Web.Mode != WebSelf {
			if e.Web.Mount == "" || strings.ContainsAny(e.Web.Mount, "/ \t") {
				panic(fmt.Sprintf("atlas: verb %q web mount %q must be one path segment",
					name, e.Web.Mount))
			}
			if reservedMounts[strings.ToLower(e.Web.Mount)] {
				panic(fmt.Sprintf("atlas: verb %q claims reserved web mount %q (reserved: %v)",
					name, e.Web.Mount, ReservedMounts()))
			}
			for other, oe := range verbs {
				if oe.Web != nil && oe.Web.Mount == e.Web.Mount {
					panic(fmt.Sprintf("atlas: verbs %q and %q both claim web mount %q",
						other, name, e.Web.Mount))
				}
			}
		}
	}
	verbs[name] = e
}

func validStage(s string) bool {
	for _, v := range Stages() {
		if s == v {
			return true
		}
	}
	return false
}

// staged places an Entry built by the managed()/provisioner() helpers on the
// SDLC spine. Those helpers predate the stage axis and are shared by many verbs
// with different stages, so the stage is applied at the call site.
func staged(stage string, e Entry) Entry {
	e.Stage = stage
	return e
}

// stageTools overrides the default StageCross for tool entries that are not
// really userland utilities. It previously corrected `foreman` — an
// orchestration command that was filed in the tool table as an import-cycle
// workaround — from StageCross to StageCode. foreman has since been suppressed
// from the public atlas (Bashy #40); sprint + weave are now the public
// assignment/orchestration surface, and foreman is an internal/compatibility
// primitive. The helper is retained for any future tool entry that needs a
// non-cross stage override.
func stageTools(stage string, names ...string) {
	for _, n := range names {
		e, ok := tools[n]
		if !ok {
			panic(fmt.Sprintf("atlas: stage %q names unknown tool %q", stage, n))
		}
		if !validStage(stage) {
			panic(fmt.Sprintf("atlas: invalid stage %q for tool %q", stage, n))
		}
		e.Stage = stage
		tools[n] = e
	}
}

// aliasTool records that one registered tool name is a second spelling of
// another (`[` is test). Both names are real registry entries — the alias
// link is what tells a consumer they are one command, not two.
func aliasTool(name, target string) {
	e, ok := tools[name]
	if !ok {
		panic(fmt.Sprintf("atlas: alias %q names unknown tool", name))
	}
	if _, ok := tools[target]; !ok {
		panic(fmt.Sprintf("atlas: alias %q targets unknown tool %q", name, target))
	}
	e.AliasOf = target
	tools[name] = e
}

// aliasVerb registers name as a second spelling of an existing verb: the
// entry is a copy of the target's classification (stage, group, tier, caps,
// effects) with AliasOf set and no web surface of its own — the console
// discovers a surface once, under the canonical name. Call it after the
// effect pass so the copy is of the final classification.
func aliasVerb(name, target string) {
	if _, dup := verbs[name]; dup {
		panic(fmt.Sprintf("atlas: alias %q is already a verb", name))
	}
	t, ok := verbs[target]
	if !ok {
		panic(fmt.Sprintf("atlas: alias %q targets unknown verb %q", name, target))
	}
	if t.AliasOf != "" {
		panic(fmt.Sprintf("atlas: alias %q targets %q, itself an alias of %q", name, target, t.AliasOf))
	}
	e := t
	e.Caps = append([]string(nil), t.Caps...)
	e.Effects = append([]string(nil), t.Effects...)
	e.OS = append([]string(nil), t.OS...)
	e.Partial = append([]string(nil), t.Partial...)
	e.AliasOf = target
	e.Web = nil
	verbs[name] = e
}

// capTools appends a capability to existing tool entries; unknown names panic
// so the tables self-check at init.
func capTools(capability string, names ...string) {
	for _, n := range names {
		e, ok := tools[n]
		if !ok {
			panic(fmt.Sprintf("atlas: cap %q names unknown tool %q", capability, n))
		}
		e.Caps = append(e.Caps, capability)
		tools[n] = e
	}
}

// shape marks the output contract of existing entries. Unknown names and
// shapes panic during atlas construction so a typo cannot silently alter
// reducer behavior.
func shape(outputShape OutputShape, names ...string) {
	if outputShape != ShapeVerdict && outputShape != ShapeResult {
		panic(fmt.Sprintf("atlas: invalid output shape %q (one of %v)", outputShape, Shapes()))
	}
	for _, n := range names {
		if e, ok := tools[n]; ok {
			e.Shape = outputShape
			tools[n] = e
			continue
		}
		if e, ok := verbs[n]; ok {
			e.Shape = outputShape
			verbs[n] = e
			continue
		}
		panic(fmt.Sprintf("atlas: shape %q names unknown command %q", outputShape, n))
	}
}

// eff appends a security effect to existing entries (tool OR verb); an unknown
// name panics so the classification self-checks at init and can never silently
// skip a command.
func eff(effect string, names ...string) {
	for _, n := range names {
		if e, ok := tools[n]; ok {
			e.Effects = append(e.Effects, effect)
			tools[n] = e
			continue
		}
		if e, ok := verbs[n]; ok {
			e.Effects = append(e.Effects, effect)
			verbs[n] = e
			continue
		}
		panic(fmt.Sprintf("atlas: effect %q names unknown command %q", effect, n))
	}
}

func sortedKeys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func init() {
	// In-process tools (tier userland by definition). The coverage test
	// asserts this set == tool.Names() with cmds/all + cmds/graph +
	// cmds/resources registered (the public Bashy inventory).
	addTools(GroupFileutils,
		"basename", "chcon", "chgrp", "chmod", "chown", "clip", "cp",
		// cygpath/wslpath convert path SPELLINGS (pure string work over the
		// shared mvdan.cc/sh/v3/pathconv converter); they never touch the
		// filesystem, like basename/dirname.
		"cygpath", "wslpath",
		"dd",
		"df", "dir", "dircolors", "dirname", "du", "file", "find", "install", "link",
		"ln", "ls", "mkdir", "mkfifo", "mknod", "mktemp", "mv", "readlink",
		"pax",
		"realpath", "rm", "rmdir", "shred", "stat", "sync", "tar", "touch",
		"tree", "truncate", "unlink", "vdir",
	)
	addTools(GroupTextutils,
		"awk", "b2sum", "base32", "base64", "basenc", "cat", "cksum", "cmp",
		"comm", "csplit", "cut", "diff", "expand", "fmt", "fold", "grep", "iconv",
		"gunzip", "gzip", "head", "hexdump", "join", "jq", "md5sum", "more",
		"nl", "numfmt", "od", "paste", "pr", "ptx", "sed", "sha1sum",
		"sha224sum", "sha256sum", "sha384sum", "sha512sum", "shuf", "sort",
		"split", "strings", "sum", "tac", "tail", "tee", "tokens", "tr",
		"tsort", "unexpand", "uniq", "uudecode", "uuencode", "wc", "xargs", "zcat",
	)
	addTools(GroupShellutils,
		"arch", "at", "atq", "atrm", "batch", "cal", "crontab",
		"date", "duration", "echo", "env", "expr", "factor", "false", "getconf",
		"groups", "hostid", "hostname", "id", "kill", "logname", "mesg", "ncal", "nice",
		"nohup", "nproc", "ntp", "pathchk", "pinky", "printenv", "printf", "ps", "pwd", "renice",
		// tput answers terminal-capability questions from the terminfo database;
		// write sends a message to another logged-in user's terminal.
		"tput", "write",
		// locale reports the environment's locale settings; logger writes to the
		// system log; newgrp changes the caller's group credential for a new shell.
		"locale", "logger", "newgrp",
		// tabs emits the terminal's clear-tab and set-tab capabilities; like tput
		// it reads terminfo and writes only to stdout.
		"tabs",
		"seq", "sleep", "sntp", "stdbuf", "stty", "test", "time",
		"timeout", "true", "tty", "tz", "uname", "uptime", "users", "watch",
		"which", "who", "whoami", "yes",
		// `[` is test under its bracket spelling — one implementation,
		// both names, exactly as upstream ships them.
		"[",
	)
	aliasTool("[", "test")
	addTools(GroupCodeIntel, "ast", "graph")
	addTools(GroupNet, "browser", "fetch")
	// foreman was here (GroupOrch) but is now a suppressed internal (Bashy #40);
	// sprint + weave are the public orchestration surface. See packages.go.
	addTools(GroupDiagnostics, "resources", "why")

	// ed is an in-process Go applet. The remaining pinned POSIX external
	// providers live in pkg/posixprovider and cmds/posixproviders.
	// These are NOT Go applets: the multicall owns the NAME and executes a
	// locally built, provenance-checked copy of the upstream program. They are
	// listed here because they are registered tools — the ratchet is about the
	// registry, not about who wrote the implementation — and their Subclass says
	// which kind they are.
	addTools(GroupToolchains, "make", "ar", "nm", "strip")
	addTools(GroupTextutils, "ed", "patch", "m4", "ex", "vi")
	addTools(GroupShellutils, "bc", "man", "localedef")
	addTools(GroupCodeIntel, "ctags")
	// lp submits a print job through cupsd. mail/mailx use local durable mailbox
	// files; talk uses authenticated ephemeral AF_UNIX IPC. They are communication
	// tools, but the local-only applets do not acquire the network effect.
	addTools(GroupNet, "lp", "mail", "mailx", "talk")
	aliasTool("mail", "mailx")
	// `posix-providers` is the provisioner in front of them: it is the ONLY
	// command that downloads and compiles one.
	addTools(GroupToolchains, "posix-providers")
	// `posix-gate` is the fail-closed effective-owner gate over the 116
	// POSIX-required names: registry ownership, provider pins/provenance, and
	// the staged runtime's PATH/shell/POSIXLY_CORRECT selection.
	addTools(GroupDiagnostics, "posix-gate")

	// Tool capabilities (evidence per flag: docs/command-atlas.md §2.3).
	capTools(CapJSON,
		"ast", "graph",
		"browser", "fetch", "duration", "tz", "ntp", "sntp", "tokens",
		"resources", "why",
	)
	capTools(CapDryRun, "rm")
	capTools(CapDestructive, "rm", "dd", "shred", "truncate", "mail", "mailx")
	capTools(CapReadOnly,
		"cat", "cmp", "comm", "df", "diff", "du", "file", "grep", "head", "hexdump",
		"ls", "od", "ps", "readlink", "realpath", "resources", "stat", "strings", "tac", "tail",
		"test", "[", "tokens", "tree", "wc", "which",
		// `ast` (symbols/search/refs/map/query) is pure structural reads.
		"ast",
		// tput and tabs only query the terminfo database and write the resulting
		// capability to stdout. tabs addresses the TERMINAL rather than the
		// filesystem, which still mutates nothing outliving the invocation.
		"tput", "tabs",
	)
	// The `graph` umbrella has write subcommands (note/link/observe/forget), so
	// it is not read-only; its structural reads keep a disk cache, so CapCached.
	capTools(CapCached, "graph")
	// `ast map` is the token-budgeted repo map.
	capTools(CapBudget, "tokens", "ast")
	capTools(CapNeedsNetwork, "fetch", "browser", "ntp", "sntp")
	capTools(CapSpawnsProcesses,
		"xargs", "timeout", "time", "watch", "nice", "nohup", "stdbuf", "at", "batch", "why",
		// find spawns the -exec/-ok utility (its specified behavior).
		"find",
	)

	// why is a managed external; its caps must match the managed() helper.
	capTools(CapCached, "why")
	capTools(CapSelfProvisioning, "why")
	if e, ok := tools["why"]; ok {
		e.Subclass = SubclassManagedExternal
		tools["why"] = e
	}

	// POSIX external providers: cached (the binmgr cache IS how they resolve) and
	// process-spawning (the whole tool is an argv passthrough). They are
	// deliberately NOT CapSelfProvisioning and NOT CapNeedsNetwork — a provider
	// invocation is a cache lookup that can never download or compile, and that
	// separation is the point (a build inside a certification arm would inject
	// network and toolchain variance into measured evidence).
	// bc, ed, mailx, make, patch, and talk are deliberately absent from this list: their
	// multicall names are exclusively owned by pure-Go applets.
	posixProviders := []string{
		"m4", "man", "ctags", "ar", "nm", "strip", "ex", "vi",
		"lp", "localedef",
	}
	capTools(CapCached, posixProviders...)
	capTools(CapSpawnsProcesses, posixProviders...)
	for _, n := range posixProviders {
		e := tools[n]
		e.Subclass = SubclassManagedExternal
		tools[n] = e
	}
	// The provisioner is the one that reaches the network and runs a compiler.
	capTools(CapCached, "posix-providers")
	capTools(CapSelfProvisioning, "posix-providers")
	capTools(CapNeedsNetwork, "posix-providers")
	capTools(CapSpawnsProcesses, "posix-providers")
	// The gate's runtime subcommand spawns the staged shell it interrogates;
	// it mutates nothing and never downloads or compiles.
	capTools(CapSpawnsProcesses, "posix-gate")
	if e, ok := tools["posix-providers"]; ok {
		e.Subclass = SubclassProvisioner
		tools["posix-providers"] = e
	}
	// foreman was a CapDaemon + StageCode tool entry here, but has been
	// suppressed from the public atlas (Bashy #40). Its implementation
	// (pkg/foreman, cmds/foreman) remains as an internal/compatibility
	// primitive; sprint + weave are the public orchestration surface.

	// Front-door verbs. Shell builtins and registry CLIs are merged by the
	// embedder (bashy); everything else lives here.
	managed := func(group, tier string, caps ...string) Entry {
		return Entry{Group: group, Tier: tier, Subclass: SubclassManagedExternal,
			Caps: append([]string{CapCached, CapSelfProvisioning, CapSpawnsProcesses}, caps...)}
	}
	provisioner := func(group string, caps ...string) Entry {
		return Entry{Group: group, Tier: TierUserland, Subclass: SubclassProvisioner,
			Caps: append([]string{CapCached, CapSelfProvisioning, CapSpawnsProcesses}, caps...)}
	}

	// orchestration
	addVerb("weave", Entry{Stage: StageCode, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON}})
	addVerb("agentic", Entry{Stage: StageCross, Group: GroupPlatform, Tier: TierUserland,
		Caps: []string{CapJSON, CapSpawnsProcesses}, Effects: []string{EffExec}})
	addVerb("sprint", Entry{Stage: StagePlan, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON},
		Web: &WebSurface{Label: "Sprint", Mount: "sprint", Mode: WebInProcess, DefaultOn: true}})
	// `dag --serve` HAS a browser view, but proxying it through the launcher does
	// not work properly yet, so it declares no WebSurface and gets no tile. The
	// declaration is what the launcher discovers, so removing it is the whole
	// change — re-add it when the surface is actually usable. A tile that opens
	// something broken is worse than no tile: it costs a click to learn nothing.
	addVerb("dag", Entry{Stage: StageCross, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON}})
	addVerb("sdlc", Entry{Stage: StageDeploy, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON}})
	// Chat began as a one-shot launcher and was temporarily renamed `invoke`.
	// It now owns real governed interactive sessions and their control surface,
	// so chat is canonical again; invoke remains a compatibility spelling.
	addVerb("chat", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("invoke", Entry{Stage: StageCode, Group: GroupOrch, AliasOf: "chat", Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("delegate", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses}})
	// genie is the local-model SWE agent's front door: it runs the genie
	// bundle's solve recipe, which starts its own model server, pulls the
	// model once and lets the agent edit the current repository. No spend:
	// the model is local, which is the point of the verb.
	addVerb("genie", Entry{Stage: StageCode, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON, CapSpawnsProcesses}})
	// ycode is the engine for agents declared in YAML (Sprint #301): bashy
	// runs its CLI in-process. Like chat/delegate it drives a model (spend) and
	// the agent's commands (exec, write), over the network when the model is
	// remote.
	addVerb("ycode", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("coach", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("meet", Entry{Stage: StagePlan, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses},
		Web: &WebSurface{Label: "Meet", Mount: "meet", Mode: WebInProcess, Port: 8637,
			Start: []string{"meet", "serve"}, DefaultOn: true}})
	addVerb("supervise", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapSpawnsProcesses}})
	addVerb("capability", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// leaderboard: the account of what the fleet actually did, where capability
	// is the routing input. Read-only over the run ledger; CROSS because you
	// ask "who has earned this" at any stage.
	addVerb("leaderboard", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// stats is a pure filter over results JSONL (resolve rates with clustered
	// CIs, paired differences, pass^k, cost per solve): it decides nothing and
	// only reads its input.
	addVerb("stats", Entry{Stage: StageTest, Group: GroupOrch, Caps: []string{CapJSON}})
	// mb: the host message board — read what was posted to you, post to others.
	// CROSS, because you check the board at any stage. NOT named inbox/im: this
	// is a shared append-only spool with per-reader cursors, so it is neither a
	// private mailbox nor push-delivered chat; those words stay reserved.
	// The board earns a browser surface on the §2 test: stateful, long-lived,
	// and read by a human far more often than written by one. The CLI is a
	// CURSOR (capped at -n for anything not addressed to you); the panel is the
	// scan across every lane, which is the one thing the CLI is not trying to
	// be. No Port and no Start: nothing to supervise, the console serves it.
	addVerb("mb", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON},
		Web: &WebSurface{Label: "Messages", Mount: "mb", Mode: WebInProcess, DefaultOn: true}})
	addVerb("messages", Entry{Stage: StageCross, Group: GroupOrch, AliasOf: "mb", Caps: []string{CapJSON}})
	// ping is an arity-selected front door: board reads/sends share mb's durable
	// local store, while a bare host or system-ping option execs the platform
	// ping. It is not an alias because both branches are intentional API.
	addVerb("ping", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapSpawnsProcesses}})
	// inbox and notify are the private receive/send faces of the same bus. They
	// remain separate top-level primitives so the atlas can show composition
	// without inventing another transport or address-book concept.
	// The inbox's web surface is the THIRD-PERSON view the CLI cannot offer:
	// `bashy inbox` fixes the filter to one reader and advances that reader's
	// cursor, so there is no invocation of it that answers "what is waiting for
	// everyone on this host". The PANEL is read-only by construction (pkg/bus's
	// InspectInbox) because an observer that consumed mail would eat it at
	// somebody else's next turn boundary — but the VERB is not, so no
	// CapReadOnly here: the ordinary `bashy inbox` read writes a cursor, and a
	// capability flag that describes only one of a verb's surfaces is a claim
	// the other surface falsifies.
	addVerb("inbox", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON},
		Web: &WebSurface{Label: "Inbox", Mount: "inbox", Mode: WebInProcess, DefaultOn: true,
			Tip: "every agent's waiting mail, read-only"}})
	addVerb("notify", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// handoff/resume: pause a live session and pass the work on -- to another
	// agentic tool, a scheduler, or tomorrow. CROSS, because you hand off work
	// at any stage: a half-finished plan, a half-finished refactor, a half-run
	// test campaign, a half-done deploy.
	addVerb("handoff", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	addVerb("resume", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// bus — the agent notification bus (publish/watch). Replaces the never-
	// reachable `notify`: the subscriber could not be `bashy watch`, because that
	// name belongs to the classic watch(1), so both halves live under one parent.
	addVerb("bus", Entry{Stage: StageCode, Group: GroupOrch, Caps: []string{CapJSON}})
	// herald — reach an agent that is NOT on this host, over A2A.
	//
	// The stage question this verb has to answer ("which stage do you serve
	// that nothing else already does?" — the one `fanout` could not answer) is
	// CROSS, and the distinguishing property is not the stage but the
	// PARTICIPANT: every other coordination verb resolves a participant through
	// capability.ResolveTool to a binary HERE. herald is the only path to a
	// capability that does not exist on this host and cannot be installed on it.
	// Tier is userland because it operates a client on this node — an external
	// third party is neither `sphere` (your own machines) nor `cloud` (a
	// hyperscaler control plane).
	addVerb("herald", Entry{
		Stage: StageCross, Group: GroupOrch, Tier: TierUserland,
		Caps: []string{CapJSON, CapNeedsNetwork, CapSpawnsProcesses},
	})

	// the fleet registry: what this host runs with. NOUNS ARE SINGULAR — see
	// the naming rule at the end of init (aliasVerb): `bashy agent list` is the
	// registry, `bashy agent add` mints one, and `agents` is only the hidden
	// plural spelling that keeps old callers working. `agent` also carries the
	// identity helper (`agent whoami`) that used to be a verb of its own — one
	// noun, one front door.
	//
	// All four registry nouns — tool, model, agent, and skill (under knowledge,
	// below) — carry `rm`, and `rm` deletes a local-store entry with no undo:
	// destructive, on every one of them, and declared the same way on every one
	// of them (operator decision D4). Rows that disagreed here would tell a policy
	// engine that `skill rm` is safer than `tool rm`, which is not true.
	addVerb("tool", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON, CapDestructive}})
	addVerb("model", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON, CapDestructive}})
	addVerb("agent", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON, CapDestructive}})
	addVerb("person", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	addVerb("whois", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	addVerb("schedule", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("act", staged(StageTest, managed(GroupOrch, TierSandbox)))
	addVerb("act-runner", staged(StageTest, managed(GroupOrch, TierSandbox, CapDaemon)))
	addVerb("mirror", staged(StageCross, managed(GroupStorage, TierUserland, CapDaemon, CapNeedsNetwork)))

	// knowledge
	addVerb("kb", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON}})
	addVerb("search", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapNeedsNetwork}})
	addVerb("sota", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapNeedsNetwork, CapSpawnsProcesses}})
	// lexicon: what do this project's words mean HERE? It PROJECTS the atlas and the
	// fleet registry into the channels agents read -- it introduces NO new source of
	// truth, which is the test it has to keep passing. The moment it starts STORING
	// vocabulary rather than projecting it, it has become the hand-written glossary
	// that the whole data-catalog industry exists because it failed.
	addVerb("lexicon", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapReadOnly}})
	// claim: who is working in this project. CROSS -- you collide at any stage:
	// planning, coding, testing, deploying. Two agents writing one project is how
	// an untested change reaches main.
	addVerb("claim", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// steward: WHO ANSWERS FOR THIS HOST, and what actually happened on it. Exactly one
	// seat per host/user, held under a monotonic fencing epoch, over an append-only
	// hash-chained journal that outlives whoever holds it — board, status, log, history
	// and checkpoints are read-only projections of that one record.
	//
	// CROSS, and for the same reason claim is: authority is not a stage. A steward
	// crashes mid-plan, mid-refactor, mid-deploy, and the successor's first question is
	// the same in every case — who was in charge, what did they do, and which of their
	// claims did anybody actually check?
	//
	// Distinct from claim and from handoff, which is why it is a third verb rather than
	// a flag on either: claim says who is working in a PROJECT, handoff moves a working
	// TREE, and steward holds a MANDATE. Claiming the seat restores no diff and touches
	// no repository — work is a diff, a seat is not.
	addVerb("steward", Entry{Stage: StageCross, Group: GroupOrch, Caps: []string{CapJSON}})
	// skill: destructive for the same reason as tool/model/agent above — `rm`
	// on the local ring, no undo.
	addVerb("skill", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapDestructive}})
	addVerb("craft", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapReadOnly}})
	// recall was a top-level verb until 2026-08-05 and is now `kb recall` — the
	// cross-ring read surface mounted under the noun that owns memory. It has no
	// entry of its own because the atlas catalogues VERBS and a subcommand is
	// not one (`kb search` has no entry either), and because the e2e dispatch
	// gate asserts every advertised verb actually runs: an entry for a verb that
	// no longer dispatches is exactly the kind of stale advertisement that gate
	// exists to catch. Its local-first property is unchanged and now rides kb's
	// entry — neither declares `net`, which pkg/atlas/localfirst_test.go pins.
	addVerb("define", Entry{Stage: StageCross, Group: GroupKnowledge, Caps: []string{CapJSON, CapReadOnly}})

	// engines
	// The tier-3 container engine has FOUR spellings and ONE canonical name
	// (operator, 2026-09-13, Sprint 167): `oci` is the STANDARD name — the O3
	// pillar (ollama · oci · otel), the spec the engine implements — and the
	// canonical entry; `sandbox` is the POPULAR name (the tier-3 venue word,
	// visible alias); `podman` and `docker` are VENDOR spellings (hidden
	// aliases, kept for callers). Dispatch still lands on the podman engine
	// (engineAlias in bashy); the atlas records who is a spelling of whom.
	addVerb("oci", Entry{Stage: StageCross, Group: GroupEngines, Tier: TierSandbox,
		Caps: []string{CapDaemon, CapSpawnsProcesses}})
	addVerb("podman", Entry{Stage: StageCross, Group: GroupEngines, Tier: TierSandbox, AliasOf: "oci",
		Caps: []string{CapDaemon, CapSpawnsProcesses}})
	addVerb("docker", Entry{Stage: StageCross, Group: GroupEngines, Tier: TierSandbox, AliasOf: "oci",
		Caps: []string{CapDaemon, CapSpawnsProcesses}})
	// `sandbox` is the TIER NAME (tier 3), so the tier vocabulary and the verb
	// surface agree: someone who reads "tier 3 = sandbox" and types it gets the
	// engine instead of "command not found". It is the same alias as `docker`.
	//
	// The word is load-bearing elsewhere and this alias does NOT carry that
	// meaning: outpost's `sandbox` app is a FILTERED libpod endpoint that strips
	// privileged/host-namespace/host-bind/added-cap requests and injects resource
	// caps, deliberately distinct from raw podman passthrough. This verb is the
	// raw local engine — it refuses nothing. Anyone extending it toward the
	// filtered semantics should make that a real capability, not a rename.
	addVerb("sandbox", Entry{Stage: StageCross, Group: GroupEngines, Tier: TierSandbox, AliasOf: "oci",
		Caps: []string{CapDaemon, CapSpawnsProcesses}})
	addVerb("ollama", Entry{Stage: StageCross, Group: GroupEngines, Tier: TierSphere,
		Caps: []string{CapDaemon, CapNeedsNetwork, CapSpawnsProcesses}})
	// `peer` is the CANONICAL name of the sphere tier's front door (operator,
	// 2026-09-13, Sprint 167) — the word a user reaches for — and `sphere` is
	// its hidden alias, kept because the tier is called that in
	// docs/execution-tiers.md. Same rule as the container engine: canonical =
	// the taught name (oci), alias = the other spellings.
	addVerb("peer", Entry{Stage: StageDeploy, Group: GroupEngines, Tier: TierSphere,
		Caps: []string{CapNeedsNetwork, CapNeedsPairing, CapSpawnsProcesses}})

	// forge
	addVerb("git", staged(StageCross, managed(GroupForge, TierUserland, CapNeedsNetwork)))
	addVerb("git-scm", staged(StageCross, provisioner(GroupForge, CapNeedsNetwork)))
	addVerb("gh", staged(StageCross, managed(GroupForge, TierUserland, CapNeedsNetwork)))
	// Same as dag: loom serves a UI, but the proxied tile is not right yet.
	addVerb("loom", staged(StageCross, managed(GroupForge, TierWorkspace, CapDaemon)))

	// net
	addVerb("web", Entry{Stage: StageCross, Group: GroupNet, Caps: []string{CapJSON}})

	// The app launcher: one port, one nav, one auth, with every other web surface
	// deep-linked beneath it — the shape docs/agent-interaction-surfaces-design.md
	// settled on, where a verb's --web-ui is a MODIFIER that deep-links in here
	// rather than standing up another server.
	//
	// StageCross because it renders whatever the operator is doing, at whatever
	// stage — it takes no position on the spine itself, it is the window onto the
	// ones that do.
	//
	// The name is the macOS register (Apps / Terminal / Files), chosen by the user
	// over `web-console`. Note outpost serves an unrelated GET /apps (its
	// cooperative-app advertisement); they share a word, not a namespace.
	// The verb is `app` (nouns are singular; `apps` is the hidden plural
	// alias); the human-facing label stays "Apps".
	addVerb("app", Entry{Stage: StageCross, Group: GroupPlatform,
		Caps: []string{CapDaemon, CapJSON, CapSpawnsProcesses},
		Web: &WebSurface{Label: "Apps", Mode: WebSelf, Port: 22749,
			Start: []string{"app"}, DefaultOn: true}})
	addVerb("curl", staged(StageCross, provisioner(GroupNet, CapNeedsNetwork)))

	// toolchains (self-provisioning, agent-mode shims)
	for _, n := range []string{
		"go", "cmake", "clang", "zig", "node", "npm", "npx", "pnpm", "yarn",
		"python", "pip", "uv", "mise", "cargo", "rustc", "rustup", "rust",
	} {
		// A compiler/package-manager is a CODE-stage tool: it is how the thing
		// gets built. (`bashy go` also runs tests, but so does every compiler —
		// the stage is what the verb is FOR, not every use it can be put to.)
		e := staged(StageCode, provisioner(GroupToolchains))
		if n == "rust" {
			e.AliasOf = "rustc"
		}
		addVerb(n, e)
	}

	// storage
	addVerb("rclone", staged(StageCross, managed(GroupStorage, TierUserland, CapNeedsNetwork)))
	addVerb("zot", staged(StageCross, managed(GroupStorage, TierUserland, CapDaemon)))
	addVerb("seaweedfs", staged(StageCross, managed(GroupStorage, TierUserland, CapDaemon)))
	addVerb("kopia", staged(StageCross, managed(GroupStorage, TierUserland, CapDaemon)))

	// cluster
	addVerb("kubectl", staged(StageDeploy, managed(GroupClusterCloud, TierCluster, CapJSON, CapNeedsNetwork)))
	addVerb("helm", staged(StageDeploy, managed(GroupClusterCloud, TierCluster, CapNeedsNetwork)))
	addVerb("dks", Entry{Stage: StageDeploy, Group: GroupClusterCloud, Tier: TierCluster,
		Caps: []string{CapDaemon, CapNeedsNetwork, CapSpawnsProcesses}})

	// platform
	// commands is the lister AND, since Sprint 179, the CRUD front door of the
	// registered-command ring (`commands add|set|rm|…`): rm deletes a local
	// entry outright (destructive, like the other registry nouns), add/set
	// write it, and `verify` on a download: record provisions over the net.
	addVerb("commands", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapDestructive, CapJSON}})
	// inspect: the one diagnostics verb whose SUBJECT IS BASHY ITSELF. `doctor`
	// answers about the host, `why` about the process tree, `otel` about a
	// trace, `audit` about one store; inspect is the resource map (where every
	// store bashy owns lives, scope-resolved for THIS cwd), the gate decisions
	// (each middleware gate with the SIGNAL that decided it), and the index that
	// names which verb answers every other question about bashy. Read-only,
	// offline, model-free by contract — a new read-only self-view is a new
	// ASPECT of this verb, never a new verb. `doctor`, `context` and `audit`
	// fold behind it (subject = bashy AND effects = {read}); the old names stay
	// as hidden aliases, like `upgrade` → `self`.
	addVerb("inspect", Entry{Stage: StageCross, Group: GroupDiagnostics, Caps: []string{CapJSON, CapReadOnly}})
	addVerb("context", Entry{Stage: StageCross, Group: GroupPlatform, AliasOf: "inspect", Caps: []string{CapJSON, CapReadOnly}})
	addVerb("doctor", Entry{Stage: StageCross, Group: GroupDiagnostics, AliasOf: "inspect", Caps: []string{CapReadOnly}})
	addVerb("otel", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapJSON, CapReadOnly, CapNeedsNetwork}})
	addVerb("audit", Entry{Stage: StageCross, Group: GroupPlatform, AliasOf: "inspect", Caps: []string{CapJSON, CapReadOnly}})
	addVerb("check", Entry{Stage: StageTest, Group: GroupDiagnostics, Caps: []string{CapJSON, CapReadOnly}})
	// gate: THE Test verb. Before it, the Test stage was EMPTY -- not because
	// nobody tested, but because the gate (the command that decides pass/fail)
	// was spelled four incompatible ways across four packages: weave's
	// suite-gate file, sdlc's healthcheck: key, supervise's :: string, and a
	// dag target that happens to fail. All four mean the same thing; they only
	// disagreed about where the command lives. This is the one place it lives.
	// The Plan stage's task tracker (subsumes the old `issue` register). Auto-scoped:
	// inside a git repo it is THAT repo's committed docs/todo/ (the structured
	// replacement for an ad-hoc TODO.md); otherwise the per-host/user personal list
	// (~/.bashy/todo/<owner>/). `weave add --from-todo` seeds a run from a repo todo.
	// No forge, no cloud — a committed directory of one file per item.
	addVerb("todo", Entry{Stage: StagePlan, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON}})
	// judge is gate's SEMANTIC twin: gate asks "does it PASS" (mechanical,
	// reproducible); judge asks "is it GOOD" (an LLM opinion, advisory unless
	// --gate). Together they finally encode "sandbox-green is not mergeable".
	addVerb("pair", Entry{Stage: StageTest, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("judge", Entry{Stage: StageTest, Group: GroupOrch, Tier: TierWorkspace, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("gate", Entry{Stage: StageTest, Group: GroupDiagnostics, Caps: []string{CapJSON, CapSpawnsProcesses}})
	// conform: BASHY'S OWN fidelity batteries (bash-5.3 compat / POSIX conformance /
	// VSC-PCTS compliance / benchmark). Renamed from `verify` 2026-07-12: it had
	// claimed the most general word in the vocabulary for the narrowest possible
	// thing — verifying BASHY ITSELF. A project that ADOPTS bashy would reach for
	// `bashy verify` to ask "does MY code pass?" and get bash's conformance suites.
	// The general pass/fail question is `bashy gate`.
	addVerb("conform", Entry{Stage: StageTest, Group: GroupDiagnostics, Caps: []string{CapSpawnsProcesses}})
	addVerb("verify", Entry{Stage: StageTest, Group: GroupDiagnostics, AliasOf: "conform", Caps: []string{CapSpawnsProcesses}})
	addVerb("self", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapCached, CapNeedsNetwork}})
	addVerb("bootstrap", Entry{Stage: StageCross, Group: GroupPlatform, AliasOf: "self",
		Caps: []string{CapCached, CapNeedsNetwork}})
	addVerb("upgrade", Entry{Stage: StageCross, Group: GroupPlatform, AliasOf: "self",
		Caps: []string{CapCached, CapNeedsNetwork}})
	addVerb("run", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapJSON, CapSpawnsProcesses}})
	addVerb("secret", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapNeedsNetwork, CapNeedsPairing}})
	// ask declares NO caps beyond --json, and that is the point of it being its own
	// verb rather than `secrets input`. Caps say what a verb REQUIRES: `secrets`
	// genuinely requires a network and a paired cloudbox, because every one of its
	// subcommands is a vault RPC. Prompting the human at this keyboard requires
	// neither, and filing it under a verb that claims otherwise would make the
	// atlas lie — an agent on an air-gapped host reads these caps and would skip a
	// capability that works perfectly there.
	addVerb("ask", Entry{Stage: StageCross, Group: GroupPlatform, Caps: []string{CapJSON}})

	// account
	addVerb("tessaro", Entry{Stage: StageCross, Group: GroupAccount, Tier: TierAccount,
		Caps: []string{CapNeedsNetwork, CapNeedsPairing, CapSpawnsProcesses}})
	addVerb("login", Entry{Stage: StageCross, Group: GroupAccount, Tier: TierAccount,
		Caps: []string{CapNeedsNetwork, CapNeedsPairing, CapSpawnsProcesses}})

	// These commands answer a pass/fail question. Their successful exit status
	// is the useful result, and an empty stdout is expected. Everything else
	// remains result-shaped (including unclassified/future entries).
	shape(ShapeVerdict, "check", "conform", "false", "gate", "judge", "true", "verify")

	// --- security-effect classification ------------------------------------
	//
	// What each command can DO, from a security/privacy/governance lens. Runs
	// last, over BOTH tables, because eff() resolves a name in either. The
	// coverage ratchet requires ≥1 effect on every entry, so a new command that
	// is added without a line here fails the build by name — classification is
	// mandatory, never fail-open. A command legitimately lists several atoms.

	// pure — deterministic, touches nothing governed.
	eff(EffPure,
		"basename", "dirname", "dircolors", "cal", "ncal", "duration", "echo",
		"expr", "factor", "false", "printf", "true", "numfmt", "seq", "sleep",
		"yes", "sync",
		// path-spelling converters: deterministic string transforms; the one
		// environment read is the /tmp mapping consulting the host temp dir.
		"cygpath", "wslpath",
	)

	// read — reads filesystem, host state, or input data (the privacy surface).
	eff(EffRead,
		// fileutils/inspection
		"df", "du", "file", "ls", "dir", "vdir", "resources", "stat", "readlink", "realpath", "tree",
		"find", "clip",
		// test/[ answer questions ABOUT files (existence, type, permission,
		// ownership, timestamps) — a filesystem read, never a write.
		"test", "[",
		// textutils (transform/read input)
		"awk", "cat", "cmp", "comm", "csplit", "cut", "diff", "expand", "fmt",
		"fold", "grep", "gzip", "gunzip", "head", "hexdump", "iconv", "join", "jq",
		"more", "nl", "od", "paste", "pr", "ptx", "sed", "shuf", "sort",
		"split", "strings", "tac", "tail", "tee", "tokens", "tr", "tsort",
		"unexpand", "uniq", "uudecode", "uuencode", "wc", "xargs", "zcat",
		"b2sum", "cksum", "md5sum", "sha1sum", "sha224sum", "sha256sum",
		"sha384sum", "sha512sum", "sum", "base32", "base64", "basenc",
		// handoff READS the working tree (the diff + untracked files it captures);
		// resume READS the record. Both are a privacy surface: a handoff record
		// carries real source, so treat it like the working tree it came from.
		"handoff", "resume",
		// host info
		"arch", "getconf", "groups", "hostid", "hostname", "id", "logname", "mesg", "nproc",
		"pathchk", "pinky", "ps", "pwd", "renice", "tty", "tz", "uname", "uptime", "users",
		// pax READS an archive or the tree it is packing; it also writes (below).
		"pax",
		// tput and tabs read the terminfo database; write reads the utmp/utmpx
		// login database to find the recipient's terminal (it also writes, below).
		"tput", "tabs", "write",
		// locale reports settings resolved from the environment and the locale
		// database. It answers questions; it changes nothing.
		"locale",
		"which", "who", "whoami", "atq", "date", "env", "printenv", "ntp",
		"sntp",
		// code-intel / net
		"ast", "graph", "browser", "fetch",
		// verbs that read stores / remote state
		"capability", "leaderboard", "stats", "meet", "mb", "messages", "ping", "inbox", "bus", "agent", "tool", "model", "person", "whois",
		"kb", "skill", "lexicon", "claim", "git", "web", "rclone", "kopia", "commands", "context",
		// craft READS the attestation ledger skills writes; it never writes it.
		"craft", "define",
		"doctor", "otel", "audit", "check", "sprint",
		// inspect READS every store it maps and never writes one — its contract.
		"inspect",
		// app READS every store its panels render, and the Files panel reads the
		// filesystem under its scope — the whole point of the tile.
		"app",
		// steward READS the host's authority record (status/board/log/history/reconcile)
		// and WRITES it (below). A privacy surface: the journal is a durable account of
		// what agents did on this machine, and its transcripts can carry real
		// conversation.
		"steward",
		// todo READS the task list (`list`/`show`) — a repo's docs/todo/ or the per-host
		// list — and WRITES it (below). A privacy surface: it is a durable account of
		// what the steward/user is doing across every thread.
		"todo", "why",
	)

	// write — mutates the filesystem or host state (short of irreversible loss).
	eff(EffWrite,
		// todo writes the task list: a repo's COMMITTED docs/todo/ (a write here lands
		// in the repo's history) or the per-host personal list (~/.bashy/todo/<owner>/).
		"todo", "why",
		"clip", "cp", "install", "kill", "link", "ln", "mkdir", "mkfifo", "mknod",
		"mktemp", "mv", "pax", "rmdir", "tar", "touch",
		// mesg flips the terminal's group-write bit; renice changes a running
		// process's scheduling priority. Both mutate host state, neither loses data.
		"mesg", "renice",
		// write writes to ANOTHER user's terminal device - host state outside
		// this process, so it is a write even though nothing on disk changes.
		"write",
		// logger appends a record to the system log - host state, off-process.
		"logger",
		"awk", "csplit", "gzip", "gunzip", "sed", "split", "tee", "uudecode", "graph",
		"stty", "atrm", "crontab",
		// handoff WRITES a portable record; resume WRITES the captured working
		// tree back into a checkout. Both also read (below). Neither execs: v1
		// dispatch PRINTS the command to launch a successor rather than spawning
		// one behind the user's back.
		"handoff", "resume",
		// ask WRITES the answered value to a 0600 file (its default destination) and
		// a metadata record while a request is pending. It never writes the value
		// anywhere the caller chose without validating the path first.
		"ask",
		// verbs
		"weave", "genie", "ycode", "sprint", "dag", "sdlc", "supervise", "capability", "leaderboard", "agent", "dks",
		"tool", "model", "person", "kb", "skill", "lexicon", "claim", "mirror", "git",
		"git-scm", "gh", "curl", "helm", "self", "bootstrap", "upgrade",
		// commands add/set write the registered-command ring (Sprint 179).
		"commands",
		"rclone", "meet", "mb", "messages", "ping", "inbox", "bus", "notify",
		// app WRITES through its terminal (a real shell) and its session key.
		"app",
		// steward APPENDS to the host's journal and rewrites the seat/grant files. It is
		// write, not destroy: the one thing that removes bytes (`steward repair`) refuses
		// anything but a torn final append, and quarantines the exact bytes it discards
		// BEFORE truncating, so nothing it does is irreversible.
		"steward",
	)

	// destroy — can IRREVERSIBLY lose data.
	eff(EffDestroy, "dd", "mail", "mailx", "rm", "shred", "truncate", "unlink",
		// The registry nouns' `rm` deletes a local-store entry outright; the
		// four rows declare it alike (see the tool/model/agent entries).
		"tool", "model", "agent", "skill",
		// commands rm deletes a registered command's local entry the same way.
		"commands",
	)

	// net — opens a network connection (the egress / exfiltration surface).
	eff(EffNet,
		"ntp", "sntp", "browser", "fetch", "search", "ping",
		"delegate", "coach", "sdlc", "chat", "invoke", "meet", "pair", "judge", "tool", "model", "agent", "act", "sota",
		"herald", "genie", "ycode",
		"act-runner", "mirror", "oci", "podman", "docker", "sandbox", "ollama", "dks", "peer", "git",
		"git-scm", "gh", "loom", "web", "curl", "rclone", "zot", "seaweedfs",
		"kopia", "kubectl", "helm", "self", "bootstrap", "upgrade", "secret",
		"otel", "tessaro", "login", "app",
		// commands verify on a download: record provisions the pinned binary.
		"commands",
	)

	// exec — spawns a process bashy no longer governs (the coreutils userland,
	// the advisor, and the audit hook do not reach across an execve).
	eff(EffExec,
		// newgrp REPLACES itself with a new shell carrying a changed group
		// credential; the shell is beyond anything bashy governs.
		"newgrp", "ping",
		"find", "awk", "xargs", "at", "batch", "nice", "nohup",
		"stdbuf", "time", "timeout", "watch", "env",
		"weave", "dag", "sdlc", "delegate", "coach", "chat", "invoke", "meet", "pair", "judge", "supervise", "schedule", "act", "sota", "genie", "ycode",
		"act-runner", "skill", "oci", "podman", "docker", "sandbox", "ollama", "dks", "peer",
		"git-scm", "loom", "curl", "zot", "seaweedfs", "kopia", "kubectl",
		"verify", "conform", "gate", "run", "tessaro", "login", "why",
		// app spawns a bashy per browser terminal tab
		"app",
		// herald runs the GATE — an operator-supplied command that decides
		// whether a peer's returned work is good.
		"herald",
	)

	// cred — reads or writes credentials / secrets. `env`/`printenv` are here
	// because they emit the whole environment, secrets included — the reason the
	// context-redaction allowlist must also cover them.
	eff(EffCred, "env", "printenv", "git", "git-scm", "gh", "secret", "ask", "tessaro", "login")

	// priv — changes privilege, ownership, or a security label.
	// newgrp changes the caller's GROUP CREDENTIAL before spawning the shell -
	// a privilege change, and the reason it is classified beyond a plain exec.
	eff(EffPriv, "chcon", "chgrp", "chmod", "chown", "install", "mknod", "newgrp")

	// remote — executes on ANOTHER host (crosses the machine boundary). `dag`
	// pipes a Host:-tagged target body to a remote `bash -s`; sphere runs pooled
	// compute on peers; mirror/rclone push to a remote endpoint.
	// herald delegates a TASK to an agent on someone else's infrastructure —
	// the machine boundary is the whole point of the verb.
	eff(EffRemote, "dag", "mirror", "peer", "rclone", "kubectl", "helm", "herald")

	// persist — leaves something that OUTLIVES the session: a cron entry, a
	// daemon, an installed/upgraded binary.
	eff(EffPersist,
		"at", "batch", "crontab", "nohup",
		"schedule", "act-runner", "mirror", "oci", "podman", "docker", "sandbox", "ollama", "dks", "meet", "mb", "messages", "ping", "inbox", "bus", "notify",
		"loom", "zot", "seaweedfs", "kopia", "self", "bootstrap", "upgrade",
		// an `app` server outlives the shell that started it.
		"app",
		// genie pulls models into the shared store and installs its bundle.
		"genie",
	)

	// spend — incurs metered cost: paid inference the agent drives, pooled
	// compute, or cloud resources.
	// judge SPENDS: every reviewer is a metered inference call, and a --panel 3
	// costs three of them. An agent must be able to see that before it fans out.
	eff(EffSpend, "delegate", "coach", "chat", "invoke", "meet", "pair", "judge", "supervise", "sdlc", "weave", "peer", "ollama", "sota", "herald", "ycode")

	// The toolchain provisioners each download over the network and then run
	// arbitrary code (a compiler / package manager / interpreter — npm and pip
	// run install scripts), so they are net+exec+write as a class.
	for _, n := range []string{
		"go", "cmake", "clang", "zig", "node", "npm", "npx", "pnpm", "yarn",
		"python", "pip", "uv", "mise", "cargo", "rustc", "rustup", "rust",
	} {
		eff(EffNet, n)
		eff(EffExec, n)
		eff(EffWrite, n)
	}

	// Every remaining POSIX external provider is exec: the tool resolves the
	// cached binary and hands it argv. ed, mailx, patch, and talk are pure-Go
	// local read/write applets and are classified separately.
	eff(EffExec, "make", "bc", "m4", "man", "ctags", "ar", "nm", "strip", "ex", "vi",
		"lp", "localedef")
	eff(EffRead, "ed", "mail", "mailx", "patch", "talk")
	eff(EffRead, "make", "bc", "m4", "man", "ctags", "ar", "nm", "strip", "ex", "vi",
		// lp reads the file it submits; localedef reads the charmap and locale definition.
		"lp", "localedef")
	eff(EffWrite, "ed", "mail", "mailx", "patch", "talk")
	eff(EffPersist, "mail", "mailx")
	eff(EffWrite, "make", "ctags", "ar", "strip", "ex", "vi",
		// localedef writes the compiled locale into the locale path.
		"localedef")
	// lp hands a job to the print scheduler. Local-only mailx and talk never
	// contact an MTA, talkd, or another host; talk's AF_UNIX session is not
	// classified as network egress.
	eff(EffNet, "lp")
	// The provisioner downloads pinned upstream source, runs a compiler over it,
	// and installs a binary that outlives the session.
	eff(EffNet, "posix-providers")
	eff(EffExec, "posix-providers")
	eff(EffWrite, "posix-providers")
	eff(EffPersist, "posix-providers")
	eff(EffRead, "posix-providers")
	// posix-gate reads the provider cache/provenance and staged PATH entries,
	// and execs the staged shell for its runtime probes.
	eff(EffRead, "posix-gate")
	eff(EffExec, "posix-gate")

	// --- origin + posix -------------------------------------------------------
	//
	// Provenance is stamped once the subclass passes are final and BEFORE the
	// alias pass, so `agents` inherits `agent`'s origin. See origin.go.
	classifyOrigins()
	// Platform support (OS / Partial), same placement and for the same reason.
	classifyPlatforms()

	// --- number aliases ------------------------------------------------------
	//
	// NOUNS ARE SINGULAR. A front-door verb that names a kind of thing is spelled
	// in the singular (`agent`, `model`, `skill`, `secret`); enumeration is the
	// `list` subcommand, never an -s suffix. English number is not a shell
	// concept: an agent cannot derive from the noun whether bashy wanted `todo`
	// or `todos`, `agent` or `agents` — it had to memorise each one, and the
	// surface had both `agent` and `agents` meaning UNRELATED things. So the
	// plural is accepted as a HIDDEN alias of the singular (kept for existing
	// callers, listed only under `commands --all`), never canonical, never
	// taught. `issue` is the same shape: the work-tracking model made it one
	// record with `todo` at two scopes, so it aliases rather than 127s.
	//
	// Runs AFTER the effect classification so an alias copies its target's
	// final effects — an alias that declared its own would be a claim the
	// target could falsify. Exceptions to the rule are enumerated, not implied:
	// POSIX/bash/GNU names are frozen by the standard (`jobs`, `dirs`, `times`,
	// `strings`, `users`, `groups`, `command`…), and `commands` stays plural
	// because `command` IS one of them and it is a subcommand-less lister of
	// the `jobs` shape. naming_test.go ratchets all of this.
	aliasVerb("agents", "agent")
	aliasVerb("models", "model")
	aliasVerb("tools", "tool")
	aliasVerb("people", "person")
	aliasVerb("skills", "skill")
	aliasVerb("secrets", "secret")
	aliasVerb("apps", "app")
	aliasVerb("issue", "todo")

	// `sphere` is the hidden alias of `peer` (see the peer entry above); the
	// embedder lists peer and hides sphere.
	aliasVerb("sphere", "peer")

	// Deterministic ordering for every consumer.
	for n, e := range tools {
		sort.Strings(e.Caps)
		sort.Strings(e.Effects)
		tools[n] = e
	}
	for n, e := range verbs {
		sort.Strings(e.Caps)
		sort.Strings(e.Effects)
		verbs[n] = e
	}
}
