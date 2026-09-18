// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import (
	"fmt"
	"sort"
)

// THE PACKAGE CENSUS — the axis that makes a capability with no verb VISIBLE.
//
// The atlas has always catalogued NAMES: a name you can type (a tool in the
// registry, a front-door verb) gets a group, a tier, an SDLC stage and an effect
// set, and the ratchet in atlas_coverage_test.go fails by name if a registered
// name has no entry.
//
// That leaves one hole, and it is the hole the whole §2.2a discipline exists to
// close. §2.2a asks every capability: *which SDLC stage do you serve that
// nothing else already does?* `bashy fanout` was deleted because it had no
// answer. But the question is only ever ASKED of things that have a name — so a
// capability that ships as a PACKAGE and never acquires a verb is never asked
// anything at all. It is not classified as "cross-cutting", it is not classified
// as "unjustified": it is INVISIBLE, and invisible is the one state the ratchet
// cannot argue with.
//
// Two live cases proved it:
//
//   - pkg/room — the same-host discovery-and-connection substrate (membership +
//     timeline + CtlSock) that `chat sessions`, `chat timeline`, `bus` and
//     `coach attach` are all built on. Five packages import it. It has no verb,
//     so it had no atlas entry, so nothing ever asked it the question.
//   - pkg/acp — a complete ACP client with ZERO importers in this repo. It is
//     the exact `fanout` condition (capability wired to nothing), reached the
//     other way round: fanout at least had a name to delete.
//
// So the census classifies every directory under pkg/ — not by guessing what a
// package is FOR, but by recording where it SURFACES. Four roles, and the two
// that mean "no verb of my own" must say why in Note. A new package under pkg/
// now fails the ratchet BY NAME until somebody writes that sentence, which is
// the smallest possible version of being asked the §2.2a question.
//
// What the census deliberately does NOT do is grow anybody's surface. Recording
// that pkg/room is a library whose front door is `chat` is not the same change
// as shipping `room list` / `room observe` (that is P0.5 of
// docs/agent-room-mesh-design.md, and it is a real design question about whether
// an operator should observe a room directly). This axis only guarantees that
// the question gets asked out loud.

// Package roles (closed vocabulary).
const (
	// RoleCommand — the package IS a command's implementation. It answers §2.2a
	// through that command's Stage, which addVerb already forced it to declare.
	RoleCommand = "command"
	// RoleLibrary — a real capability with NO command of its own, reached only
	// through another command. It answers §2.2a through its FrontDoor's stage,
	// and Note must say why it does not deserve a verb.
	RoleLibrary = "library"
	// RoleSupport — plumbing with no capability to place on the spine at all: a
	// regexp engine, a pty wrapper, a price table. Nothing to ask.
	RoleSupport = "support"
	// RoleUnwired — a capability that reaches NO surface: no command, no front
	// door, no importer. This is the `fanout` condition. It is a role rather
	// than an omission on purpose: the point of the census is that this state
	// has to be WRITTEN DOWN, where a reviewer sees it, instead of being the
	// silent default it was for pkg/acp.
	RoleUnwired = "unwired"
)

// Package is one pkg/<name> directory's census record.
type Package struct {
	Role string
	// FrontDoor is the atlas command name through which this package's
	// capability is reachable. Required for command/library, empty otherwise.
	// It must resolve in Lookup, so a front door cannot be wishful.
	FrontDoor string
	// Note is required for library and unwired: WHY there is no command of this
	// package's own. It is the §2.2a answer in prose for the cases where there
	// is no Stage field to carry it.
	Note string
}

// Roles returns the closed package-role vocabulary, sorted.
func Roles() []string {
	return []string{RoleCommand, RoleLibrary, RoleSupport, RoleUnwired}
}

// LookupPackage returns the census record for a pkg/<name> directory.
func LookupPackage(name string) (Package, bool) {
	p, ok := packages[name]
	return p, ok
}

// PackageNames returns the censused package names, sorted.
func PackageNames() []string {
	out := make([]string, 0, len(packages))
	for n := range packages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// PackagesWithRole returns the censused packages holding the given role, sorted.
// `PackagesWithRole(RoleUnwired)` is the standing list of capability that
// currently reaches nothing — the thing §2.2a is for.
func PackagesWithRole(role string) []string {
	var out []string
	for n, p := range packages {
		if p.Role == role {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func cmdPkg(front string) Package { return Package{Role: RoleCommand, FrontDoor: front} }

func libPkg(front, note string) Package {
	return Package{Role: RoleLibrary, FrontDoor: front, Note: note}
}

func supPkg(note string) Package { return Package{Role: RoleSupport, Note: note} }

// packages is the census. The ratchet asserts it is exactly the set of
// directories under pkg/ — no missing entry, no stale one.
var packages = map[string]Package{
	// --- command implementations -------------------------------------------
	// These answer §2.2a through their command's Stage. Nothing more is owed
	// here: addVerb/stageTools already refused to let them ship unplaced.
	"agentcmd":   libPkg("agent", "the identity helper mounted as `agent whoami` — the launcher-stamped agent principal, resolved the way the board resolves it"),
	"admission":  libPkg("bus", "store-neutral deterministic byte budgeting, priority projection, overflow digests, and prepared acknowledgements behind Bus/chat turn preambles; it owns no messages and therefore has no command surface"),
	"ask":        cmdPkg("ask"),
	"board":      libPkg("sprint", "read-only machine-global projection behind the Sprint web panel and steward/conductor dashboards; it no longer owns a top-level verb"),
	"bscript":    libPkg("agentic", "bounded command/script-to-skill yield lowering behind the agentic front door; it intentionally has no independently executable surface"),
	"browser":    cmdPkg("browser"),
	"bus":        cmdPkg("bus"),
	"capability": cmdPkg("capability"),
	"chat":       cmdPkg("chat"),
	"codegraph":  cmdPkg("graph"),
	"dag":        cmdPkg("dag"),
	"foreman":    libPkg("weave", "internal process manager for steerable agent sessions under weave and sprint; suppressed from the public atlas (Bashy #40)"),
	"gate":       cmdPkg("gate"),
	"handoff":    cmdPkg("handoff"),
	"herald":     cmdPkg("herald"),
	"judge":      cmdPkg("judge"),
	"kb":         cmdPkg("kb"),
	"ref": libPkg("define", "the ONE grammar for naming anything bashy can address — `<kind>:<id>`, "+
		"`urn:dhnt:<kind>:<id>`, the closed 15-kind vocabulary, the node every store's resolver returns. "+
		"Stdlib-only leaf by test, because pkg/kb already reaches fleet/bus/principal and the shared type "+
		"had to sit below all of them. Its verb is `define <ref>` (pkg/lexicon reads the registry the "+
		"shell fills); kb's prose parser consumes the vocabulary. Sprint 168, docs/uniform-ref-addressing.md."),
	"recall": libPkg("kb", "the cross-ring read surface — 'what is known about X' across kb pages AND craft "+
		"capabilities, one envelope, per-ring caps, never composing. It owned the top-level verb `bashy recall` "+
		"until 2026-08-05; it is now `bashy kb recall`, because four days of telemetry caught the only agent that "+
		"ever reached for it unprompted typing exactly that, and being told the verb did not exist"),
	"lexicon":    cmdPkg("lexicon"),
	"meet":       cmdPkg("meet"),
	"webconsole": cmdPkg("app"),
	"webterm": libPkg("app", "the browser terminal: one pty per websocket, running bashy itself. "+
		"Separate from webconsole so the platform-conditional pty code is one small package — "+
		"and so outpost's /shell can later collapse onto it instead of keeping a second implementation"),
	"websession": libPkg("app", "stateless HMAC session cookie + per-IP login throttle for a locally "+
		"served web surface; copied from outpost's adminui, which is internal/ and so unimportable"),
	"svcd": libPkg("app", "the start/status/stop daemon lifecycle a supervised bashy service exposes: "+
		"pidfile, a port probe that identifies the listener via /healthz before signalling it, and a "+
		"stop that escalates so the port is actually freed. Adopted by the web console first; pkg/meet, "+
		"pkg/sdlc and pkg/schedule still carry private copies and should migrate onto it"),
	"mirror":        cmdPkg("mirror"),
	"pair":          cmdPkg("pair"),
	"patch":         cmdPkg("patch"),
	"posixprovider": cmdPkg("posix-providers"),
	"repomap":       cmdPkg("ast"),
	"resources":     cmdPkg("resources"),
	"schedule":      cmdPkg("schedule"),
	"sdlc":          cmdPkg("sdlc"),
	"search":        cmdPkg("search"),
	"secrets":       cmdPkg("secret"),
	"skills":        cmdPkg("skill"),
	"craft":         cmdPkg("craft"),
	"role": libPkg("sprint", "how to REACH whoever holds a role — the bus topic and room behind the "+
		"contact `bashy sprint` shows and `bashy steward` leads with. role is vocabulary only "+
		"(pkg/steward imports it and sits in the cross-OS canary that meet cannot satisfy); "+
		"role/meetroom opens and closes the rooms, and sweeps the ones whose holder died without "+
		"releasing. Not a verb: a channel is named by the thing it reaches."),
	"redact": libPkg("craft", "strips host identity (hostnames, users, home paths, IPs, MACs, emails) before learned knowledge leaves the machine"),
	"execlog": libPkg("graph",
		"the agentic command history — the TIME plane behind `graph history`. Records "+
			"every dispatched command, redacted and capped, on the paths the bash "+
			"`history` builtin structurally cannot see (script, -c, and the agent "+
			"ExecHandler). Not a verb of its own: it is one layer of the knowledge "+
			"graph, not a second graph, and `history` is already taken by a builtin "+
			"that must stay bash-exact."),
	"spacegraph": libPkg("graph",
		"the entity graph behind `graph space` / `graph reached` — the SPACE plane. "+
			"Hosts, endpoints and accounts, and the relations observed between them, "+
			"accumulated over time. Not a verb of its own, and deliberately no export "+
			"path: every node names something real about somebody's machine, which "+
			"makes it a fact store, and facts never leave the host."),
	"sota":       cmdPkg("sota"),
	"steward":    cmdPkg("steward"),
	"supervise":  cmdPkg("supervise"),
	"timezones":  cmdPkg("tz"),
	"todo":       cmdPkg("todo"),
	"treesitter": cmdPkg("ast"),
	"who": libPkg("who", "PID-liveness for the bashy-owned login records `who` reads — a login row is only true "+
		"while its process is alive, so a stale row would invite a `write` to a name that will never read it"),
	"weave":      cmdPkg("weave"),
	"webinspect": cmdPkg("browser"),

	// --- libraries: real capability, no verb of its own ---------------------

	// THE CASE THIS AXIS WAS ADDED FOR.
	//
	// room is the discovery-and-connection substrate: membership (who is live on
	// this host, with a pid the store prunes on read), an append-only timeline,
	// and the CtlSock that makes a member reachable. pkg/ask, pkg/bus, pkg/chat,
	// pkg/kb and pkg/weave all build on it; `chat sessions`, `chat timeline`,
	// `chat steer` and `coach attach` are its operator-visible face.
	//
	// It is recorded as a LIBRARY, not registered as a verb, and the reason is
	// not squeamishness — it is that a verb here would be a LIE about dispatch.
	// The atlas's verb table is asserted against the live cobra registry (bashy
	// merges it in); an addVerb("room", …) with no `bashy room` behind it would
	// fail that coverage test, and if it somehow didn't, it would advertise to
	// every agent reading the atlas a command that does not exist. A name in the
	// atlas is a promise you can type it.
	//
	// Its §2.2a answer, for the record: room serves the CODE stage through
	// `chat` — but the honest form of the answer is that room does not serve a
	// stage, it serves a QUESTION every stage asks ("who is live here and how do
	// I reach them"), and today `chat` is the only mouth it has. Whether that is
	// the right shape is exactly P0.5 of docs/agent-room-mesh-design.md
	// (room list / room observe / room timeline, QUEUED). This entry does not
	// pre-judge it; it makes sure the question stops being invisible while it
	// waits.
	"room": libPkg("chat", "same-host membership + timeline + CtlSock substrate under "+
		"chat sessions/timeline/steer and coach attach, plus bus and weave. No verb of its own: "+
		"the verb table is asserted against live cobra dispatch, so a `room` entry with no "+
		"`bashy room` behind it would advertise a command nobody can type. An operator-facing "+
		"surface (room list/observe/timeline) is P0.5 in docs/agent-room-mesh-design.md — QUEUED, "+
		"deliberately not invented here."),

	"atlas": libPkg("commands", "the catalog itself, projected by `bashy commands` and the MCP "+
		"list_tools metadata. No `atlas` verb ON PURPOSE: a catalog that is also a command would "+
		"be a second source of truth about the command surface, which is the failure mode the "+
		"whole package exists to prevent."),
	"ctty": libPkg("ask", "the channel ladder (controlling terminal → GUI askpass → nothing) under "+
		"`bashy ask`. Not a capability an operator invokes — it is HOW ask reaches a human, and it "+
		"is meaningless without a question to carry."),
	"fleet": libPkg("tool", "the declarative registry behind tool/model/agent/person/whois. Four "+
		"verbs project one registry; the registry is not a fifth verb."),
	"hostauth": libPkg("app", "verifies web-console login credentials against the host OS. "+
		"No verb of its own: authentication is a guard on the apps surface, not an independently "+
		"invokable capability."),
	"policy": libPkg("inspect", "policy/audit is the tamper-evident record behind `bashy inspect audit`; "+
		"policy/coord is the same-project collision guard behind `bashy claim`. Two capabilities, "+
		"two existing front doors, no `policy` verb — a verb here would be a settings surface, and "+
		"policy is enforced, not configured."),
	"principal": libPkg("whois", "name → the thing it names, plus how to reach it. Every verb that "+
		"addresses an agent resolves through it; `whois` is the one that shows its work."),
	"spacetime": libPkg("inspect", "where-and-when this process is running, reported by "+
		"`bashy inspect context`. A measurement, not an action."),
	"coopauth": libPkg("login", "the ONE shared cloudbox/outpost cooperative-auth implementation. "+
		"It has no surface of its own by design: auth that you can invoke directly is auth you can "+
		"invoke around."),
	"llmbudget": libPkg("chat", "the local-first meter and gate under every metered-inference "+
		"verb (chat/judge/meet/coach/pair). Deliberately not a verb: a budget an agent can call "+
		"is a budget an agent can raise. It is read through the verbs that declare EffSpend."),
	"otelquery": libPkg("otel", "the query half of `bashy otel`, split from pkg/telemetry so "+
		"reading traces does not link the exporter."),
	"telemetry": libPkg("otel", "bashy's OpenTelemetry voice — emission, not a command. `bashy otel` "+
		"is the operator surface over what it writes."),
	"issue": libPkg("todo", "the committed issue register, SUBSUMED by `bashy todo` (see the todo "+
		"entry in atlas.go). Retained as the store behind the todo verb; it is a live candidate for "+
		"deletion, not a capability awaiting a verb."),

	// --- support: plumbing with nothing to place on the spine ---------------
	"agentctl":    supPkg("control contract for driving a third-party agent CLI"),
	"agentlaunch": supPkg("launch-line rendering for agent CLIs"),
	"agentpty":    supPkg("runs an agent CLI attached to a pty"),
	"assetring":   supPkg("ring catalog shared by the declarative externals"),
	"autofix":     supPkg("adapts a plausible-but-wrong command into one that runs here"),
	"autoretry":   supPkg("transient-failure recognition and retry policy"),
	"binmgr":      supPkg("download → verify → cache → supervise, under every managed external"),
	"bre":         supPkg("POSIX BRE → Go regexp, shared by grep and sed"),
	"collate": supPkg("glibc ISO-8859-1 collation via dlopen'd strcoll_l (purego, no cgo); a " +
		"provider-only engine like bre, wired to no verb — locale-aware ordering a tool asks for, not a command"),
	"ctype": supPkg("glibc C/POSIX and ISO-8859-1 character classification and case mapping via " +
		"dlopen'd *_l functions (purego, no cgo); collate's sibling provider, wired to no verb — " +
		"POSIX classes and case mapping a tool asks for, not a command"),
	"ignore": supPkg("the opt-in agentic path filter shared by grep and find"),
	"jobs":   supPkg("job control for the embedding shell's builtins, which the shell owns"),
	"locale": supPkg("invocation-local POSIX locale-category precedence shared by locale-aware tools"),
	"lockfileconsumers": supPkg("the yoke half of lockfile's one-platform-pair invariant: a test-only package " +
		"asserting no consumer here (weave, meet, steward, policy) grew its own lock_unix/lock_windows"),
	"lockfile": supPkg("the ONE process file-lock primitive — Acquire/TryAcquire/AcquireWithin, " +
		"one platform pair for the whole tree. Five packages hand-rolled their own and " +
		"three of five had been ported to Windows while two were no-ops"),
	"nudge":     supPkg("the proactive half of the agent-hint subsystem; emitted, never invoked"),
	"oci":       supPkg("separate module wrapping podman's OCI bindings for external/podman"),
	"ollm":      supPkg("Ollama API client wrapper, isolating the SDK from the rest of the tree"),
	"pax":       supPkg("safe portable archive manifest and extraction-preflight kernel; deliberately has no command front door until the POSIX pax surface is complete"),
	"pricing":   supPkg("local token price catalog consumed by pkg/llmbudget"),
	"procguard": supPkg("parent-death process-group containment shared by agentpty and chat"),
	"recommend": supPkg("the shell's did-you-mean; reached on command-not-found, never typed"),
	"scope":     supPkg("the git-repo-aware store resolver shared by every store"),
	"weavecli":  supPkg("the agent-friendly CLI conventions every front-door verb tree adopts"),

	// --- unwired: capability that reaches nothing ---------------------------
	//
	// Recorded, not hidden. pkg/acp is a complete ACP client — it launches an
	// agent subprocess, drives a prompt turn, and reports the files touched —
	// and it has ZERO importers in this repository. That is the `fanout`
	// condition with the pieces swapped: fanout had a verb and no callers, acp
	// has callers' worth of capability and no name.
	//
	// This entry does not judge it and does not touch it (wiring acp is live
	// work elsewhere). It exists so that the state is a written claim somebody
	// can be wrong about, instead of a silence nobody can notice. When acp
	// acquires a front door, this becomes a command/library entry and the Note
	// goes away; if it never does, the next reader has the §2.2a conversation
	// with a record in hand.
	"acp": {Role: RoleUnwired, Note: "ACP (Agent Client Protocol) client: launches an agent " +
		"subprocess, drives a prompt turn, reports touched files. No importer in this repo and no " +
		"front-door verb — the `fanout` condition. Wiring it is in flight elsewhere; reclassify to " +
		"command/library when it lands, or hold the §2.2a conversation that retired fanout."},
	"mailx": {Role: RoleLibrary, FrontDoor: "mailx", Note: "pure-Go local-mail kernel used by cmds/mailx and its mail alias: " +
		"validated message parsing, mbox delivery, locking, transactional mailbox updates, and From-line quoting."},
	"release": {Role: RoleUnwired, Note: "release pipeline T0 core: .goreleaser.yaml subset, " +
		"build matrix, deterministic archives, sha256 ledger (`bashy-release-v1`). No importer and " +
		"no front-door verb yet — the `bashy release` cobra tree is the wiring that lands in bashy, " +
		"not here; reclassify to command/library when it does."},
	"reduce": {Role: RoleUnwired, Note: "command-output reduction primitive and content-addressed spill store. " +
		"NewOutCmd is not mounted and no execution seam imports the package yet; the Sprint 123 chat and shell " +
		"seam stories own that wiring. Reclassify when the first front door lands."},
}

func init() {
	// Self-check at init, matching the rest of the atlas's discipline: a role
	// outside the vocabulary, or a library/unwired record that skipped the
	// "why", should not be able to start the binary.
	for n, p := range packages {
		if !validRole(p.Role) {
			panic(fmt.Sprintf("atlas: package %q has role %q, want one of %v", n, p.Role, Roles()))
		}
		switch p.Role {
		case RoleCommand, RoleLibrary:
			if p.FrontDoor == "" {
				panic(fmt.Sprintf("atlas: package %q (%s) names no front-door command", n, p.Role))
			}
		default:
			if p.FrontDoor != "" {
				panic(fmt.Sprintf("atlas: package %q (%s) must not name a front door (%q)",
					n, p.Role, p.FrontDoor))
			}
		}
		if p.Role != RoleCommand && p.Note == "" {
			panic(fmt.Sprintf("atlas: package %q (%s) has no note. Why does it have no command "+
				"of its own? That sentence IS its §2.2a answer.", n, p.Role))
		}
	}
}

func validRole(r string) bool {
	for _, v := range Roles() {
		if r == v {
			return true
		}
	}
	return false
}
