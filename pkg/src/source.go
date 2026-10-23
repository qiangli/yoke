package src

import (
	"fmt"
	"sort"

	"github.com/qiangli/yoke/pkg/atlas"
)

// Module paths for src show's Go package field.
const (
	yokeModule      = "github.com/qiangli/yoke"
	coreutilsModule = "github.com/qiangli/coreutils"
)

// Info is one command's source record: what `src show` prints.
type Info struct {
	Command string
	Repo    string
	Package string
	Class   Class
	Commit  string
	// ProtectedParts lists the protected manifest rows inside the command's
	// own package: the guard-rail files its implementation ships with. The
	// command's class is unaffected — protection is per path.
	ProtectedParts []ProtEntry
}

// coreVerbSource hand-maps the 14 locked core verbs to their source. The
// class comes from ClassOf; only repo/package are curated here, and every
// yoke package named was verified to exist in this repo.
var coreVerbSource = map[string][2]string{
	"agent":    {"yoke", yokeModule + "/pkg/agentcmd"},
	"model":    {"yoke", yokeModule + "/pkg/fleet"},
	"tool":     {"yoke", yokeModule + "/pkg/toolcmd"},
	"skill":    {"yoke", yokeModule + "/pkg/skills"},
	"self":     {"bashy", ""},
	"src":      {"yoke", yokeModule + "/pkg/src"},
	"git":      {"managed", "managed:git"},
	"dag":      {"yoke", yokeModule + "/pkg/dag"},
	"commands": {"yoke", yokeModule + "/pkg/fleet"},
	"inspect":  {"bashy", ""},
	"secret":   {"yoke", yokeModule + "/pkg/secrets"},
	"ask":      {"yoke", yokeModule + "/pkg/ask"},
	"claim":    {"yoke", yokeModule + "/pkg/steward"},
	"resource": {"yoke", yokeModule + "/pkg/resources"},
}

// extraSource covers non-core commands whose census front door needs no
// lookup: sprint's rod lives with weave's (NewSprintCmd in pkg/weave).
var extraSource = map[string][2]string{
	"sprint": {"yoke", yokeModule + "/pkg/weave"},
	"weave":  {"yoke", yokeModule + "/pkg/weave"},
}

// yokeCmds are the tool front doors implemented in this repo's cmds/
// multicall tree (verified against the cmds/ directory). A yoke-owned tool
// resolves here before the census.
var yokeCmds = map[string]bool{
	"all": true, "ast": true, "atq": true, "atrm": true,
	"browser": true, "cal": true, "clip": true, "db": true,
	"duration": true, "fetch": true, "foreman": true, "graph": true,
	"gzip": true, "hexdump": true, "jq": true, "lens": true,
	"mount": true, "nft": true, "ntp": true, "resources": true,
	"tar": true, "tokens": true, "tree": true, "tz": true,
	"watch": true, "which": true, "why": true, "winpath": true,
}

// posixProviderTools are the ten POSIX external providers from the atlas
// tool table (managed-external subclass): cached binaries resolved through
// the posix-providers provisioner, never linked code.
var posixProviderTools = map[string]bool{
	"m4": true, "man": true, "ctags": true, "ar": true, "nm": true,
	"strip": true, "ex": true, "vi": true, "lp": true, "localedef": true,
}

// Source maps cmd to its source repo and Go package path. It fails loudly
// on unknown names: a source map that invents provenance is worse than
// none. Derivation order:
//
//  1. the curated core-verb and extra tables;
//  2. bash builtins (the shell wins dispatch, so the shell owns them);
//  3. atlas externals: POSIX providers vs bin-managed;
//  4. the census: a cmdPkg front door names its yoke package;
//  5. yoke cmds/ tools, then yoke's own (bashy-origin) tools;
//  6. everything else in-process: the certified coreutils module.
func Source(cmd string) (repo, pkg string, err error) {
	if s, ok := coreVerbSource[cmd]; ok {
		return s[0], s[1], nil
	}
	if s, ok := extraSource[cmd]; ok {
		return s[0], s[1], nil
	}
	if IsBuiltin(cmd) {
		return "bashy", "builtin", nil
	}
	e, ok := atlas.Lookup(cmd)
	if !ok {
		return "", "", fmt.Errorf("src: unknown command %q", cmd)
	}
	if e.Origin == atlas.OriginExternal {
		if posixProviderTools[cmd] || cmd == posixProviderFront {
			return "posix-provider", "provider:" + cmd, nil
		}
		return "managed", "managed:" + cmd, nil
	}
	if dir, ok := censusCommand(cmd); ok {
		return "yoke", yokeModule + "/pkg/" + dir, nil
	}
	if yokeCmds[cmd] {
		return "yoke", yokeModule + "/cmds/" + cmd, nil
	}
	if e.Origin == atlas.OriginBashy {
		return "yoke", yokeModule, nil
	}
	return "coreutils", coreutilsModule, nil
}

// censusCommand inverts the atlas package census: the pkg/ directory whose
// command role names cmd as its front door. Only command roles count — a
// library's front door is the command that reaches it, not its address.
func censusCommand(cmd string) (string, bool) {
	var found string
	for _, dir := range atlas.PackageNames() {
		p, ok := atlas.LookupPackage(dir)
		if !ok || p.Role != atlas.RoleCommand || p.FrontDoor != cmd {
			continue
		}
		if found == "" || dir < found {
			found = dir
		}
	}
	return found, found != ""
}

// CensusPackageNames is the sorted census directory list, for tests.
func CensusPackageNames() []string {
	out := atlas.PackageNames()
	sort.Strings(out)
	return out
}

// packageRelDir converts a Source package path to the repo-relative directory
// its protected_parts are drawn from. The second result is false when the
// package is unknown (a shell-mounted command with no Go package), in which
// case there are no parts to list.
func packageRelDir(repo, pkg string) (string, bool) {
	if pkg == "" {
		return "", false
	}
	switch repo {
	case "yoke":
		if pkg == yokeModule {
			return "", true
		}
		const prefix = yokeModule + "/"
		if len(pkg) > len(prefix) && pkg[:len(prefix)] == prefix {
			return pkg[len(prefix):], true
		}
		return "", false
	case "coreutils":
		if pkg == coreutilsModule {
			return "", true
		}
		return "", false
	default:
		return "", false
	}
}

// Show returns the full `src show` record for cmd.
func Show(cmd string) (Info, error) {
	repo, pkg, err := Source(cmd)
	if err != nil {
		return Info{}, err
	}
	commit := ReleaseCommit()
	if commit == "" {
		commit = "unknown"
	}
	var parts []ProtEntry
	if dir, ok := packageRelDir(repo, pkg); ok {
		parts = PackageProtectedParts(repo, dir)
	}
	return Info{Command: cmd, Repo: repo, Package: pkg, Class: ClassOf(cmd), Commit: commit, ProtectedParts: parts}, nil
}
