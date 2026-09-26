// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// NOUNS ARE SINGULAR. A front-door verb that names a kind of thing is spelled
// in the singular; enumeration is the `list` subcommand, never an -s suffix.
// English number is not a shell concept — an agent cannot derive from the
// noun whether the verb wanted `agent` or `agents`, so every plural was a fact
// to memorise and a retry when misremembered (and for a while `agent` and
// `agents` were two unrelated verbs). A plural spelling may exist only as a
// hidden alias of its singular. Exceptions are enumerated here, never implied.

// nonPluralEndingInS are verbs whose final s is not a plural. `stats` is
// short for statistics (a mass noun, the filter over results JSONL); its
// would-be singular `stat` is the coreutils file-status command.
var nonPluralEndingInS = []string{"bus", "dks", "seaweedfs", "stats", "whois"}

// pluralListers are listers of the bash `jobs`/`dirs` shape, where the
// plural means "print the set". `commands` cannot be singularised: `command`
// is a POSIX special builtin. Since Sprint 179 it also carries the CRUD words
// of the registered-command ring (a CRUD word counts only when a NAME
// follows it), and `command` is its hidden, no-shim front-door alias in bashy
// — the exception rests on the POSIX collision alone.
var pluralListers = []string{"commands"}

// irregularPlurals cannot be seen by the suffix check.
var irregularPlurals = []string{"people"}

func isPluralName(n string) bool {
	if slices.Contains(irregularPlurals, n) {
		return true
	}
	return strings.HasSuffix(n, "s") && !slices.Contains(nonPluralEndingInS, n)
}

func TestVerbsAreSingular(t *testing.T) {
	for _, n := range atlas.VerbNames() {
		if !isPluralName(n) || slices.Contains(pluralListers, n) {
			continue
		}
		e, _ := atlas.Lookup(n)
		if e.AliasOf == "" {
			t.Errorf("%s: plural verb with no alias_of — the canonical spelling is the singular; "+
				"add the singular as the real entry and aliasVerb(%q, <singular>), or declare the "+
				"exception in naming_test.go with its reason", n, n)
			continue
		}
		if isPluralName(e.AliasOf) {
			t.Errorf("%s → %s: alias target is itself plural", n, e.AliasOf)
		}
	}
	// The allowlists must stay honest: an entry that no longer exists is a
	// stale exception waiting to excuse something else.
	for _, n := range slices.Concat(nonPluralEndingInS, pluralListers) {
		if _, ok := atlas.Lookup(n); !ok {
			t.Errorf("naming_test.go allowlists %q, which is not a verb", n)
		}
	}
}

// A number alias is the SAME command under another spelling, so its
// classification must be a copy of the target's — an alias that claimed
// fewer effects than its target would let a policy gate pass on one spelling
// and refuse the other. (The older hand-written aliases — context, doctor,
// verify, bootstrap — predate aliasVerb and are not held to this; they are
// listed so a new alias cannot hide among them.)
func TestNumberAliasMetadataMatchesTarget(t *testing.T) {
	legacy := []string{"invoke", "messages", "docker", "sandbox", "rust", "context", "doctor", "audit", "verify", "bootstrap", "upgrade"}
	for _, n := range atlas.VerbNames() {
		e, _ := atlas.Lookup(n)
		if e.AliasOf == "" || slices.Contains(legacy, n) {
			continue
		}
		target, ok := atlas.Lookup(e.AliasOf)
		if !ok {
			t.Fatalf("%s: alias_of %q does not resolve", n, e.AliasOf)
		}
		if e.Stage != target.Stage || e.Group != target.Group || e.Tier != target.Tier {
			t.Errorf("%s: stage/group/tier %s/%s/%s differ from target %s: %s/%s/%s",
				n, e.Stage, e.Group, e.Tier, e.AliasOf, target.Stage, target.Group, target.Tier)
		}
		if !reflect.DeepEqual(e.Caps, target.Caps) {
			t.Errorf("%s: caps %v differ from target %s: %v", n, e.Caps, e.AliasOf, target.Caps)
		}
		if !reflect.DeepEqual(e.Effects, target.Effects) {
			t.Errorf("%s: effects %v differ from target %s: %v", n, e.Effects, e.AliasOf, target.Effects)
		}
		if e.Web != nil {
			t.Errorf("%s: an alias must not declare its own web surface (the console discovers one under %s)", n, e.AliasOf)
		}
	}
}

// The package census names the door a package is reached through; that door
// is the canonical spelling, never an alias, so `commands --atlas` and the
// lexicon teach one name.
func TestPackageFrontDoorsAreCanonical(t *testing.T) {
	for _, name := range atlas.PackageNames() {
		p, _ := atlas.LookupPackage(name)
		if p.FrontDoor == "" {
			continue
		}
		e, ok := atlas.Lookup(p.FrontDoor)
		if !ok {
			continue // TestCensusRecordsAreWellFormed owns unresolved doors
		}
		if e.AliasOf != "" {
			t.Errorf("pkg/%s: front door %q is an alias of %q — the census names the canonical spelling", name, p.FrontDoor, e.AliasOf)
		}
	}
}
