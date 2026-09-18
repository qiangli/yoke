package lexicon

// THE SYSTEM INVENTORY — jargon that is ENUMERATED, never guessed.
//
// The rest of this package projects from software registries (the atlas, the
// fleet, skills) because those are declared and maintained. The terms an
// operator actually trips over are often not declared anywhere: an env var only
// this fleet sets, a command only this host has, a path element that means
// something here and nothing in general. `WEAVE_AGENT`, `outpost`, `.agents`.
//
// The tempting way to find those is statistical — mine the logs for strings
// that look unusual. That approach has a specific and serious failure: the
// highest-scoring "unusual string" on any real machine is a credential. An
// extractor tuned to prefer rare, non-dictionary tokens is a secret harvester
// with a glossary's name on it, and persisting its output makes the leak
// indexed rather than merely present.
//
// So this enumerates instead. Every term here came from asking a subsystem what
// exists, which means:
//
//   - it is real by construction — no scoring, no guessing;
//   - a secret is never in the answer, because secrets are not in the
//     environment's KEYS, the PATH's FILENAMES, or a directory's STRUCTURE.
//
// # Keys, never values
//
// The split this file is built around. `WEAVE_AGENT` is jargon; its value may be
// anything. `gh` is a command name; its argv may carry a token. Every enumerator
// below takes the left-hand side only, and the types cannot hold the right-hand
// side — a value has nowhere to go, so none can leak by oversight.
//
// # Enumeration gives existence; observation gives salience
//
// A host has hundreds of binaries and thousands of path segments. Emitting all
// of them is noise, which is the opposite of a glossary. Enumeration answers
// "is this term REAL"; ranking it by how much it MATTERS is a separate,
// later problem — and the safe division of labour, because the risky corpora
// (traces, logs) then only rank terms that were already validated, and never
// introduce new ones.
//
// The cut applied here is the cheap, closed-vocabulary one: subtract what is
// standard. What is left over is, by definition, local.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/redact"
)

// Kinds contributed by the system inventory.
const (
	KindEnvVar       Kind = "env-var"       // an environment variable NAME
	KindCommand      Kind = "command"       // an executable on PATH that is not standard
	KindPathSegment  Kind = "path-segment"  // a directory name that carries local meaning
	KindStandardTool Kind = "standard-tool" // part of the pure-Go userland, standard everywhere
	KindAlias        Kind = "alias"         // a shell alias defined in this host's rc files
	// KindStandardEnvVar is an environment variable from the POSIX/ubiquitous
	// set that is actually SET on this host. Not local jargon — and that is
	// precisely why it is projected, for the same reason KindStandardTool is: a
	// resolver that answers "unknown here" for PATH is wrong on its own terms.
	KindStandardEnvVar Kind = "standard-env-var"
)

const (
	envScopeNote = "An environment variable this host or fleet sets. The NAME is the term; " +
		"its value is not recorded here and may be a credential."
	commandScopeNote = "An executable present on THIS host's PATH and not part of the standard " +
		"userland. On another machine the same name may be absent or mean something else."
	pathScopeNote = "A directory name that carries local meaning in this workspace, not its " +
		"ordinary English sense."
	aliasScopeNote = "A shell alias defined on THIS host. Typing the name does NOT run the " +
		"command of the same name — it runs the expansion, which may add flags that change " +
		"behaviour substantially."
	standardEnvScopeNote = "A standard environment variable, present on almost every POSIX " +
		"host — not local jargon. The NAME is the term; its value is not recorded here, and " +
		"on this host it may be anything."
)

// SystemInventory is the enumerated term set. It holds NAMES only.
//
// There is deliberately no field for a value, a path, or a full command line.
// Enforcing the keys-only rule in the type is what makes it hold under later
// edits, rather than depending on every future caller remembering it.
type SystemInventory struct {
	EnvVars []string `json:"env_vars,omitempty"`
	// StandardEnvVars are the keys from the POSIX/ubiquitous set that this host
	// actually SETS. They are not jargon, and they are collected for the same
	// reason the standard userland is: `define` is asked "what is PATH" far more
	// often than it is asked about any fleet variable, and answering "unknown
	// here" for the most standard name on the machine reads as a broken tool
	// rather than as the honest silence it is meant to be.
	//
	// Kept in a SEPARATE field rather than merged into EnvVars because the
	// glossary and the resolver want different sets: `lexicon emit` teaches
	// local jargon and must not spend its ~15-term budget on HOME, while
	// `define` must answer for both. One list could not serve both without a
	// caller-side filter, and a filter is what goes out of date.
	StandardEnvVars []string `json:"standard_env_vars,omitempty"`
	Commands        []string `json:"commands,omitempty"`
	PathSegments    []string `json:"path_segments,omitempty"`
	// Interfaces and Mounts come from Discover, which asks the OS rather than
	// the shell. Names only — an interface's ADDRESSES are identity and are
	// returned separately as Discoveries.
	Interfaces []string `json:"interfaces,omitempty"`
	Mounts     []string `json:"mounts,omitempty"`
	// CommandPaths maps a local command to where it was found, and Aliases maps
	// an alias to what it expands to. These are LOCATIONS, not terms: they
	// answer "where is it" and "what does it really run", which is the half a
	// bare name cannot.
	CommandPaths map[string]string `json:"command_paths,omitempty"`
	Aliases      map[string]string `json:"aliases,omitempty"`
}

// EnumOptions configures enumeration. Every input is injectable so the whole
// thing is hermetic under test: nothing here reads the host unless asked to.
type EnumOptions struct {
	// Environ is the environment, in "KEY=VALUE" form (os.Environ() shape).
	// Only the keys are read; values are discarded immediately.
	Environ []string
	// PathDirs are directories to scan for executables (filepath.SplitList of
	// $PATH). Unreadable entries are skipped, not fatal — a stale PATH entry is
	// normal and must not fail enumeration.
	PathDirs []string
	// Roots are directories whose segment names are candidate terms (the repo
	// root, the workspace).
	Roots []string
	// KnownCommands is the standard command set to subtract, supplied by the
	// host rather than hard-coded — the atlas knows this, and passing it in
	// keeps this package usable outside bashy (the same reason Build takes
	// synopses).
	KnownCommands []string
	// RCFiles are shell startup files to read alias definitions from.
	RCFiles []string
	// Scrubber removes identity-bearing terms. Required in practice: a home
	// directory's own path carries a username, so path segments leak identity
	// unless filtered. Nil disables the filter, which is only appropriate in
	// tests with synthetic input.
	Scrubber *redact.Scrubber
}

// standardEnvVars is the POSIX/ubiquitous set. Anything NOT here is, on an
// ordinary machine, something a person or a project chose to define — which is
// exactly the definition of local jargon.
var standardEnvVars = map[string]bool{
	"_": true, "COLUMNS": true, "DISPLAY": true, "EDITOR": true, "ENV": true,
	"HOME": true, "HOSTNAME": true, "IFS": true, "LANG": true, "LINES": true,
	"LOGNAME": true, "MAIL": true, "OLDPWD": true, "PAGER": true, "PATH": true,
	"PS1": true, "PS2": true, "PWD": true, "SHELL": true, "SHLVL": true,
	"TERM": true, "TMPDIR": true, "TZ": true, "USER": true, "VISUAL": true,
	"GOPATH": true, "GOROOT": true, "LESS": true, "MANPATH": true, "SSH_AUTH_SOCK": true,
	"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true,
	"XDG_RUNTIME_DIR": true, "XDG_STATE_HOME": true,
}

// genericSegments are directory names that mean the same thing everywhere, so
// they carry no local information and would only dilute the term set.
var genericSegments = map[string]bool{
	"bin": true, "build": true, "cmd": true, "config": true, "dist": true,
	"doc": true, "docs": true, "etc": true, "examples": true, "home": true,
	"include": true, "internal": true, "lib": true, "libexec": true, "local": true,
	"opt": true, "pkg": true, "private": true, "sbin": true, "scripts": true,
	"share": true, "src": true, "target": true, "test": true, "tests": true,
	"tmp": true, "usr": true, "var": true, "vendor": true, "node_modules": true,
	"projects": true, "users": true, "work": true, "workspace": true,
}

// Enumerate asks the system what exists, and subtracts what is standard.
func Enumerate(o EnumOptions) SystemInventory {
	var inv SystemInventory

	// --- env: KEYS only. The value is split off and dropped on the same line
	// it is produced, so it is never held in a variable that could be logged.
	for _, kv := range o.Environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		// Two buckets, one pass. Which bucket a key lands in is decided by the
		// closed standard set, never by the key's shape — so a name cannot be
		// promoted to "standard" by looking ordinary.
		switch name := strings.TrimSpace(k); {
		case isLocalEnvVar(name):
			inv.EnvVars = appendUnique(inv.EnvVars, name)
		case isStandardEnvVar(name):
			inv.StandardEnvVars = appendUnique(inv.StandardEnvVars, name)
		}
	}

	// --- PATH: executable NAMES, minus the standard userland.
	known := map[string]bool{}
	for _, c := range o.KnownCommands {
		known[strings.TrimSpace(c)] = true
	}
	for _, dir := range o.PathDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a stale or unreadable PATH entry is normal
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if known[name] || !isLocalCommandName(name) {
				continue
			}
			inv.Commands = appendUnique(inv.Commands, name)
			// FIRST match wins, exactly as PATH resolution does — reporting a
			// later one would name a binary the shell would never actually run.
			if inv.CommandPaths == nil {
				inv.CommandPaths = map[string]string{}
			}
			if _, seen := inv.CommandPaths[name]; !seen {
				inv.CommandPaths[name] = filepath.Join(dir, name)
			}
		}
	}

	// --- aliases: what a name REALLY runs.
	//
	// Worth collecting precisely because an alias is invisible: typing `codex`
	// may run `codex --sandbox danger-full-access`, and an agent reasoning about
	// what a command will do has no way to see that from the name.
	for _, rc := range o.RCFiles {
		for name, expansion := range parseAliases(rc) {
			if inv.Aliases == nil {
				inv.Aliases = map[string]string{}
			}
			if _, seen := inv.Aliases[name]; !seen {
				inv.Aliases[name] = expansion
			}
		}
	}

	// --- paths: SEGMENT names, minus the generic ones.
	for _, root := range o.Roots {
		for _, seg := range strings.Split(filepath.ToSlash(root), "/") {
			if isLocalSegment(seg) {
				inv.PathSegments = appendUnique(inv.PathSegments, seg)
			}
		}
	}

	// --- identity filter. A home directory's own path contains a username, so
	// without this the "path segments" of any real machine include the operator's
	// login. Applied last, over everything, so no enumerator can bypass it.
	//
	// StandardEnvVars is deliberately NOT filtered. It is a compile-time closed
	// set that contains no identity by construction, and running the scrubber
	// over it can only ever REMOVE a true answer: the scrubber matches this
	// host's own login and hostname case-insensitively, so an operator whose
	// username happens to be `term` or `home` would silently lose `TERM` and
	// `HOME` from the resolver. Filtering a set that cannot leak buys nothing
	// and costs correctness.
	if o.Scrubber != nil {
		inv.EnvVars = dropIdentifying(o.Scrubber, inv.EnvVars)
		inv.Commands = dropIdentifying(o.Scrubber, inv.Commands)
		inv.PathSegments = dropIdentifying(o.Scrubber, inv.PathSegments)
	}

	sort.Strings(inv.EnvVars)
	sort.Strings(inv.StandardEnvVars)
	sort.Strings(inv.Commands)
	sort.Strings(inv.PathSegments)
	return inv
}

// dropIdentifying removes terms the scrubber recognises as host identity. A term
// that IS someone's name or address is not vocabulary — it is a fact about a
// machine, and belongs in the host-local fact layer, not a shareable glossary.
func dropIdentifying(s *redact.Scrubber, terms []string) []string {
	out := terms[:0:0]
	for _, t := range terms {
		if s.Clean(t) {
			out = append(out, t)
		}
	}
	return out
}

// aliasLine matches `alias NAME=VALUE`, with or without quotes. Deliberately
// simple: this reads a declaration, it does not interpret shell, and anything
// it cannot parse is skipped rather than guessed at.
var aliasLine = regexp.MustCompile(`^\s*alias\s+([A-Za-z_][A-Za-z0-9_.-]*)=(.*)$`)

// parseAliases reads alias declarations from one shell startup file.
//
// A missing or unreadable file is normal — most hosts have only some of the
// conventional rc files — so it yields nothing rather than an error.
func parseAliases(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "#"); i == 0 {
			continue
		}
		m := aliasLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		val := strings.TrimSpace(m[2])
		// Strip one layer of matching quotes; leave anything else alone.
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') || (val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
			}
		}
		if name != "" && val != "" {
			out[name] = val
		}
	}
	return out
}

func isLocalEnvVar(name string) bool {
	if name == "" || isStandardEnvVar(name) {
		return false
	}
	// A single letter is not a term.
	return len(name) >= 3
}

// isStandardEnvVar reports a key from the POSIX/ubiquitous set — the exact
// complement of isLocalEnvVar's first rejection, factored out so the two can
// never disagree about what "standard" means. When they disagreed, a key could
// be rejected as jargon by one and not recognised as standard by the other, and
// it would simply vanish.
func isStandardEnvVar(name string) bool {
	// LC_* and similar locale families are standard by prefix.
	return standardEnvVars[name] || strings.HasPrefix(name, "LC_")
}

func isLocalCommandName(name string) bool {
	if len(name) < 2 {
		return false
	}
	// Versioned duplicates (python3.12, gcc-14) denote the same concept as their
	// base name and would triple the term set without adding meaning.
	if strings.ContainsAny(name, ".") {
		return false
	}
	return true
}

func isLocalSegment(seg string) bool {
	seg = strings.TrimSpace(seg)
	if len(seg) < 3 || genericSegments[strings.ToLower(seg)] {
		return false
	}
	// A leading dot is meaningful (.agents, .bashy) but the dot itself is not
	// part of the term.
	return true
}

// AddSystem projects an enumerated inventory into the store.
//
// A separate method rather than a parameter on Build: the system inventory is
// optional, host-specific, and slower to gather than the registry projections,
// and a caller that only wants verb resolution should not pay for a PATH scan.
func (s *Store) AddSystem(inv SystemInventory, ov Overlay) {
	for _, name := range inv.EnvVars {
		s.add(Concept{
			ID: "env:" + name, Kind: KindEnvVar, PrefLabel: name,
			Definition: "an environment variable set on this host or by this fleet",
			ScopeNote:  envScopeNote,
			Source:     "system:env",
		}, ov)
	}
	// The standard ones share the `env:` id namespace with the local ones. They
	// cannot collide — Enumerate puts every key in exactly one bucket — and one
	// namespace is what makes `bashy define env:PATH` work without the caller
	// having to know which bucket a name fell into.
	for _, name := range inv.StandardEnvVars {
		s.add(Concept{
			ID: "env:" + name, Kind: KindStandardEnvVar, PrefLabel: name,
			Definition: "a standard environment variable, set on this host",
			ScopeNote:  standardEnvScopeNote,
			Source:     "system:env",
		}, ov)
	}
	for _, name := range inv.Commands {
		s.add(Concept{
			ID: "command:" + name, Kind: KindCommand, PrefLabel: name,
			Definition: "an executable on this host's PATH, outside the standard userland",
			ScopeNote:  commandScopeNote,
			Location:   inv.CommandPaths[name],
			Source:     "system:path",
		}, ov)
	}
	// Aliases are added AFTER commands so both readings survive: a name that is
	// both an alias and a binary is genuinely two things, and which one runs
	// depends on how it is invoked. Hiding either is how a caller reasons about
	// the wrong one.
	for name, expansion := range inv.Aliases {
		s.add(Concept{
			ID: "alias:" + name, Kind: KindAlias, PrefLabel: name,
			Definition: "a shell alias on this host — typing this runs the expansion, not the bare command",
			ScopeNote:  aliasScopeNote,
			Location:   expansion,
			Source:     "system:rc",
		}, ov)
	}
	for _, name := range inv.PathSegments {
		s.add(Concept{
			ID: "path:" + name, Kind: KindPathSegment, PrefLabel: name,
			Definition: "a directory name carrying local meaning in this workspace",
			ScopeNote:  pathScopeNote,
			Source:     "system:path",
		}, ov)
	}
	s.reindex()
}

// defaultRCFiles are the shell startup files aliases are conventionally
// declared in. A path that does not exist costs nothing.
func defaultRCFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range []string{".bashrc", ".bash_profile", ".bash_aliases", ".profile", ".zshrc"} {
		out = append(out, filepath.Join(home, name))
	}
	return out
}

// AddStandardTools projects the standard userland — the tools every bashy has,
// as opposed to the local commands only this host has.
//
// They are NOT jargon, and that is exactly why they belong here. A verb called
// `define` that answers "unknown" for `ls` is wrong on its own terms: `ls` is
// applicable to this system, and an agent asking about it deserves the right
// pointer rather than a shrug. Keeping them a distinct Kind preserves the
// distinction that matters — standard everywhere vs peculiar to here — while
// still answering the question.
func (s *Store) AddStandardTools(names []string, ov Overlay) {
	for _, name := range names {
		n := strings.TrimSpace(name)
		if n == "" {
			continue
		}
		c := Concept{
			ID: "tool:" + n, Kind: KindStandardTool, PrefLabel: n,
			Definition: "a standard tool in bashy's pure-Go userland, run in-process",
			ScopeNote: "Standard everywhere bashy runs — not local jargon. It resolves " +
				"in-process rather than from PATH, so `which` may not find it.",
			Use:    "bashy commands " + n,
			Source: "atlas",
		}
		// The facet comes off the atlas record; a name the atlas does not know
		// has no effects to project, so it gets none rather than an empty one.
		if e, ok := atlas.Lookup(n); ok {
			c.Action = commandFacet(n, e, commandExecutor(e, ExecutorCoreutils))
		}
		s.add(c, ov)
	}
	s.reindex()
}

// EnumerateHost is the live-machine convenience wrapper: the real environment,
// the real PATH, and the given roots, with the host scrubber applied.
func EnumerateHost(roots []string, knownCommands []string) SystemInventory {
	return Enumerate(EnumOptions{
		Environ:       os.Environ(),
		PathDirs:      filepath.SplitList(os.Getenv("PATH")),
		RCFiles:       defaultRCFiles(),
		Roots:         roots,
		KnownCommands: knownCommands,
		Scrubber:      redact.FromHost(),
	})
}
