// Package autofix adapts a plausible-but-wrong command into one that runs HERE,
// and reports what it did — instead of returning an error the agent must spend a
// round-trip diagnosing. It is the act-half sibling of pkg/nudge (which only
// observes): when an agent uses a flag from a different shell/version/platform
// (GNU vs BSD, bash vs zsh, Linux vs macOS) on a READ-ONLY command, and the
// intended meaning is unambiguous, autofix rewrites the command to the local
// equivalent and emits a note ("`sed -r` isn't valid here; ran `sed -E` — the
// portable form — instead:").
//
// Hard safety rules:
//   - READ-ONLY commands only. A rewrite is applied pre-exec, so it must never
//     touch a command that could mutate state (no `sed -i`, no redirects the
//     table can't see — the caller gates to simple read-only commands).
//   - TRUE-ALIAS rewrites only. Every entry must be behavior-identical to what
//     the agent meant on its home platform; autofix never changes a result, only
//     the spelling that makes it run here. Anything whose semantics differ across
//     platforms (e.g. grep -P PCRE vs -E ERE) is NOT a candidate.
//   - TRANSPARENT. Every rewrite emits a note; nothing is silently changed.
//   - Reversible intent: if in doubt, do nothing and let the original run.
package autofix

import (
	"runtime"
	"strings"
)

// rule adapts one command's argv. It returns the rewritten argv and a note, or
// ok=false to leave the command untouched.
type rule func(args []string) (fixed []string, note string, ok bool)

// rules is keyed by command name (argv[0]). Each rule is a true-alias, read-only
// adaptation. Keep every entry conservative — see the package doc's safety rules.
var rules = map[string]rule{
	"sed": sedDialect,
}

// Adapt returns a rewritten argv + a note when a read-only, true-alias
// adaptation applies to args, or ok=false to leave it unchanged. Pure: argv in,
// argv out, no I/O.
func Adapt(args []string) (fixed []string, note string, ok bool) {
	if len(args) == 0 {
		return nil, "", false
	}
	r, has := rules[args[0]]
	if !has {
		return nil, "", false
	}
	return r(args)
}

// sedDialect rewrites GNU sed's `-r` (extended regexp) to the portable `-E`,
// which BSD/macOS sed requires and GNU sed also accepts — identical semantics.
// It refuses to touch a command that writes (`-i`/`--in-place`), keeping the
// adaptation read-only. Only fires where the local sed would actually reject
// `-r` (non-GNU platforms); on Linux `-r` is already valid, so nothing to fix.
func sedDialect(args []string) ([]string, string, bool) {
	if runtime.GOOS == "linux" {
		return nil, "", false // GNU sed accepts -r; no adaptation needed
	}
	if hasInPlace(args) {
		return nil, "", false // writes — never auto-adapt
	}
	changed := false
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case a == "-r" || a == "--regexp-extended":
			out[i] = "-E"
			changed = true
		case len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'r'):
			// A combined short cluster like -nr / -rn: swap the r for E, keeping
			// the other flags. (E and r both mean ERE; order-independent.)
			out[i] = "-" + strings.ReplaceAll(a[1:], "r", "E")
			changed = true
		default:
			out[i] = a
		}
	}
	if !changed {
		return nil, "", false
	}
	return out, "adapted GNU `sed -r` to the portable `sed -E` (identical extended-regexp semantics) so it runs on this platform too — prefer `-E`.", true
}

func hasInPlace(args []string) bool {
	for _, a := range args[1:] {
		if a == "-i" || a == "--in-place" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		// combined short cluster containing i, e.g. -ri
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'i') {
			return true
		}
	}
	return false
}
