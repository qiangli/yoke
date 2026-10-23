package src

import (
	"sort"

	"github.com/qiangli/yoke/pkg/atlas"
)

// Class is the invariant class axis: where a command comes from and what
// may change it. It derives from the atlas origin axis plus the core verb
// list — never from package layout or tier.
type Class string

const (
	// ClassCore is the immutable userland: bash builtins, POSIX-required and
	// GNU coreutils names, the POSIX-provider externals, and the core verbs.
	ClassCore Class = "core"
	// ClassProtected is yoke/brain surface under the protected manifest:
	// non-core, non-external commands implemented under a protected path.
	ClassProtected Class = "protected"
	// ClassSkill is everything else that ships in-process: the agent skill
	// surface (M3b owns skills proper).
	ClassSkill Class = "skill"
	// ClassExternal is bin-managed: downloaded, verified and exec'd, never
	// linked — the supply-chain policy's third-party boundary.
	ClassExternal Class = "external"
)

// Classes returns the closed class vocabulary.
func Classes() []string {
	return []string{string(ClassCore), string(ClassProtected), string(ClassSkill), string(ClassExternal)}
}

// CoreVerbs is the locked core verb set from the Sprint 458 M0 record: the
// agent-model-tool-skill identity plane plus the supply-chain and
// coordination verbs. The ratchet fails on drift — add a verb here only by
// amending the record.
func CoreVerbs() []string {
	return []string{
		"agent", "model", "tool", "skill",
		"self", "src", "git", "dag",
		"commands", "inspect", "secret", "ask",
		"claim", "resource",
	}
}

var coreVerbSet = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range CoreVerbs() {
		m[v] = true
	}
	return m
}()

// bashBuiltins is the bash 5.3 builtin set: the names the embedding shell
// resolves without forking. It is public shell knowledge (help/compgen), not
// implementation code; coreutils and the atlas never list it because the
// shell owns it and merges it in.
var bashBuiltins = map[string]bool{
	"alias": true, "bg": true, "bind": true, "break": true,
	"builtin": true, "caller": true, "cd": true, "command": true,
	"compgen": true, "complete": true, "compopt": true, "continue": true,
	"declare": true, "dirs": true, "disown": true, "echo": true,
	"enable": true, "eval": true, "exec": true, "exit": true,
	"export": true, "false": true, "fc": true, "fg": true,
	"getopts": true, "hash": true, "help": true, "history": true,
	"jobs": true, "kill": true, "let": true, "local": true,
	"logout": true, "mapfile": true, "popd": true, "printf": true,
	"pushd": true, "pwd": true, "read": true, "readarray": true,
	"return": true, "set": true, "shift": true, "shopt": true,
	"source": true, "suspend": true, "test": true, "time": true,
	"times": true, "trap": true, "true": true, "type": true,
	"typeset": true, "ulimit": true, "umask": true, "unalias": true,
	"unset": true, "wait": true,
}

// IsBuiltin reports whether cmd is a bash 5.3 shell builtin.
func IsBuiltin(cmd string) bool { return bashBuiltins[cmd] }

// IsCoreVerb reports whether cmd is one of the 14 locked core verbs.
func IsCoreVerb(cmd string) bool { return coreVerbSet[cmd] }

var gnuSet = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range atlas.GNUCoreutilsUpstream() {
		m[n] = true
	}
	return m
}()

// posixProviderFront is the provisioner in front of the POSIX external
// providers: it downloads and compiles them, so it is governed like them.
const posixProviderFront = "posix-providers"

// ClassOf returns the invariant class of cmd, derived in this order:
//
//  1. core verbs, bash builtins, POSIX-required and GNU coreutils names,
//     and the POSIX-provider front — the immutable userland;
//  2. bin-managed externals (atlas OriginExternal) — the exec'd boundary;
//  3. non-core, non-external commands implemented under a protected
//     manifest path;
//  4. everything else — the skill surface.
//
// ClassOf is total: every name classifies, so the ratchet's "no command
// lacks a class" holds by construction and the test pins the derivation.
func ClassOf(cmd string) Class {
	if coreVerbSet[cmd] || bashBuiltins[cmd] {
		return ClassCore
	}
	if atlas.IsPosixRequired(cmd) || gnuSet[cmd] || cmd == posixProviderFront {
		return ClassCore
	}
	if e, ok := atlas.Lookup(cmd); ok && e.Origin == atlas.OriginExternal {
		return ClassExternal
	}
	if isProtectedCommand(cmd) {
		return ClassProtected
	}
	return ClassSkill
}

// AtlasCommands returns every command the atlas catalogues (in-process tools
// plus front-door verbs), sorted. The ratchet iterates this set.
func AtlasCommands() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range atlas.ToolNames() {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range atlas.VerbNames() {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
