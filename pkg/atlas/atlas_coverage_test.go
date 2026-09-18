// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// The atlas coverage ratchet: the tool table must stay exactly in sync with
// the live tool registry (yoke's cmds/all — which carries coreutils' certified
// cmds/all — plus the bashy-only cmds/graph and cmds/resources), vocabularies
// are closed, and idioms reference only known commands. Adding a tool without an atlas entry — or leaving a stale
// entry behind — fails here by name.
package atlas_test

import (
	"slices"
	"sort"
	"testing"

	_ "github.com/qiangli/yoke/cmds/all"
	_ "github.com/qiangli/yoke/cmds/graph"
	_ "github.com/qiangli/yoke/cmds/resources"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

func sliceSet[T ~string](items []T) map[T]bool {
	out := make(map[T]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

func TestToolTableMatchesRegistry(t *testing.T) {
	registered := tool.Names()
	inAtlas := sliceSet(atlas.ToolNames())
	regSet := sliceSet(registered)

	for _, n := range registered {
		if !inAtlas[n] {
			t.Errorf("registered tool %q has no atlas entry (add it to pkg/atlas)", n)
		}
	}
	for _, n := range atlas.ToolNames() {
		if !regSet[n] {
			t.Errorf("atlas tool entry %q is stale: no such registered tool", n)
		}
	}
}

func TestClosedVocabularies(t *testing.T) {
	groups := sliceSet(atlas.Groups())
	tiers := sliceSet(atlas.Tiers())
	stages := sliceSet(atlas.Stages())
	shapes := sliceSet(atlas.Shapes())
	caps := sliceSet(atlas.Capabilities())
	effects := sliceSet(atlas.Effects())

	check := func(names []string) {
		for _, n := range names {
			e, ok := atlas.Lookup(n)
			if !ok {
				t.Fatalf("Lookup(%q) missing for listed name", n)
			}
			if !groups[e.Group] {
				t.Errorf("%s: group %q not in vocabulary", n, e.Group)
			}
			if !tiers[e.Tier] {
				t.Errorf("%s: tier %q not in vocabulary", n, e.Tier)
			}
			// Every entry sits on the SDLC spine. addVerb already panics at init
			// on a missing stage, so this guards the value, not the presence.
			if !stages[e.Stage] {
				t.Errorf("%s: sdlc stage %q not in vocabulary %v", n, e.Stage, atlas.Stages())
			}
			if !shapes[e.OutputShape()] {
				t.Errorf("%s: output shape %q not in vocabulary %v", n, e.OutputShape(), atlas.Shapes())
			}
			if !sort.StringsAreSorted(e.Caps) {
				t.Errorf("%s: caps not sorted: %v", n, e.Caps)
			}
			seen := map[string]bool{}
			for _, c := range e.Caps {
				if !caps[c] {
					t.Errorf("%s: cap %q not in vocabulary", n, c)
				}
				if seen[c] {
					t.Errorf("%s: duplicate cap %q", n, c)
				}
				seen[c] = true
			}

			// Security-effect classification is MANDATORY. A command with no
			// declared effect is unclassified, and unclassified must fail the
			// build, not fall through as harmless — this is the whole point of
			// the axis. EffPure is the explicit "considered, no governed effect"
			// declaration for the genuinely benign ones (true, echo, seq).
			if len(e.Effects) == 0 {
				t.Errorf("%s: no security effect declared (classify it in pkg/atlas; use %q if genuinely benign)", n, atlas.EffPure)
			}
			if !sort.StringsAreSorted(e.Effects) {
				t.Errorf("%s: effects not sorted: %v", n, e.Effects)
			}
			seenEff := map[string]bool{}
			for _, ef := range e.Effects {
				if !effects[ef] {
					t.Errorf("%s: effect %q not in vocabulary", n, ef)
				}
				if seenEff[ef] {
					t.Errorf("%s: duplicate effect %q", n, ef)
				}
				seenEff[ef] = true
			}
			// EffPure is exclusive: a command is benign OR it has real effects,
			// never both. Catching this keeps "pure" meaningful.
			if seenEff[atlas.EffPure] && len(e.Effects) > 1 {
				t.Errorf("%s: %q cannot be combined with other effects: %v", n, atlas.EffPure, e.Effects)
			}
		}
	}
	check(atlas.ToolNames())
	check(atlas.VerbNames())

	// Tools are userland by definition; tiers beyond it belong to verbs.
	for _, n := range atlas.ToolNames() {
		if e, _ := atlas.Lookup(n); e.Tier != atlas.TierUserland {
			t.Errorf("tool %s: tier %q (tools are userland by definition)", n, e.Tier)
		}
	}
}

func TestOutputShapeDefaultsToResult(t *testing.T) {
	for _, shape := range []atlas.OutputShape{"", "future", "VERDICT", atlas.ShapeResult} {
		if got := (atlas.Entry{Shape: shape}).OutputShape(); got != atlas.ShapeResult {
			t.Errorf("OutputShape(%q) = %q, want %q", shape, got, atlas.ShapeResult)
		}
	}
	if got := (atlas.Entry{Shape: atlas.ShapeVerdict}).OutputShape(); got != atlas.ShapeVerdict {
		t.Errorf("OutputShape(%q) = %q, want %q", atlas.ShapeVerdict, got, atlas.ShapeVerdict)
	}
}

func TestOutputShapeClassifiesDecisionCommands(t *testing.T) {
	for _, name := range []string{"check", "conform", "false", "gate", "judge", "true", "verify"} {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Fatalf("%s is absent from atlas", name)
		}
		if got := e.OutputShape(); got != atlas.ShapeVerdict {
			t.Errorf("%s output shape = %q, want %q", name, got, atlas.ShapeVerdict)
		}
	}
	for _, name := range []string{"[", "cmp", "diff", "find", "grep", "ls", "test", "go", "git"} {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Fatalf("%s is absent from atlas", name)
		}
		if got := e.OutputShape(); got != atlas.ShapeResult {
			t.Errorf("%s output shape = %q, want conservative %q", name, got, atlas.ShapeResult)
		}
	}
}

func TestResolveOutputShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want atlas.OutputShape
	}{
		{name: "go build", argv: []string{"go", "build"}, want: atlas.ShapeVerdict},
		{name: "go test", argv: []string{"go", "test", "./..."}, want: atlas.ShapeVerdict},
		{name: "go vet", argv: []string{"go", "vet"}, want: atlas.ShapeVerdict},
		{name: "git push", argv: []string{"git", "push"}, want: atlas.ShapeVerdict},
		{name: "cargo build", argv: []string{"cargo", "build"}, want: atlas.ShapeVerdict},
		{name: "cargo test", argv: []string{"cargo", "test"}, want: atlas.ShapeVerdict},
		{name: "npm test", argv: []string{"npm", "test"}, want: atlas.ShapeVerdict},
		{name: "go list", argv: []string{"go", "list"}, want: atlas.ShapeResult},
		{name: "git log", argv: []string{"git", "log"}, want: atlas.ShapeResult},
		{name: "git status", argv: []string{"git", "status"}, want: atlas.ShapeResult},
		{name: "git diff", argv: []string{"git", "diff"}, want: atlas.ShapeResult},
		{name: "git show", argv: []string{"git", "show"}, want: atlas.ShapeResult},
		{name: "go unknown", argv: []string{"go", "future"}, want: atlas.ShapeResult},
		{name: "unknown program", argv: []string{"future-tool", "test"}, want: atlas.ShapeResult},
		{name: "empty argv", want: atlas.ShapeResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := atlas.ResolveOutputShape(tc.argv); got != tc.want {
				t.Errorf("ResolveOutputShape(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

func TestAliasTargetsExist(t *testing.T) {
	for _, names := range [][]string{atlas.ToolNames(), atlas.VerbNames()} {
		for _, n := range names {
			e, _ := atlas.Lookup(n)
			if e.AliasOf == "" {
				continue
			}
			if _, ok := atlas.Lookup(e.AliasOf); !ok {
				t.Errorf("%s: alias_of %q does not resolve", n, e.AliasOf)
			}
		}
	}
}

func TestMailAliasSecurityMetadataMatchesMailx(t *testing.T) {
	mail, ok := atlas.Lookup("mail")
	if !ok {
		t.Fatal("mail alias is absent from atlas")
	}
	mailx, ok := atlas.Lookup("mailx")
	if !ok {
		t.Fatal("mailx is absent from atlas")
	}
	if mail.AliasOf != "mailx" {
		t.Fatalf("mail AliasOf = %q, want mailx", mail.AliasOf)
	}
	if !slices.Equal(mail.Caps, mailx.Caps) {
		t.Errorf("mail caps = %v, mailx caps = %v", mail.Caps, mailx.Caps)
	}
	if !slices.Equal(mail.Effects, mailx.Effects) {
		t.Errorf("mail effects = %v, mailx effects = %v", mail.Effects, mailx.Effects)
	}
	if !slices.Contains(mailx.Caps, atlas.CapDestructive) {
		t.Error("mailx delete-on-quit lacks destructive capability")
	}
	if !slices.Contains(mailx.Effects, atlas.EffDestroy) {
		t.Error("mailx delete-on-quit lacks destroy security effect")
	}
}

// The board family is local-file communication: reads consume durable logs,
// sends append to them, and ordinary reads/drains advance per-reader cursors.
// Keep those storage effects visible to policy without misclassifying the
// transport as network egress or an external process.
func TestLocalBoardCommunicationSecurityMetadata(t *testing.T) {
	for _, name := range []string{"mb", "messages", "inbox", "bus"} {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Errorf("%s is absent from atlas", name)
			continue
		}
		for _, effect := range []string{atlas.EffRead, atlas.EffWrite, atlas.EffPersist} {
			if !slices.Contains(e.Effects, effect) {
				t.Errorf("%s effects = %v, missing %q for durable local communication", name, e.Effects, effect)
			}
		}
		for _, effect := range []string{atlas.EffNet, atlas.EffExec, atlas.EffRemote} {
			if slices.Contains(e.Effects, effect) {
				t.Errorf("%s effects = %v, local-file communication must not declare %q", name, e.Effects, effect)
			}
		}
	}
	mb, _ := atlas.Lookup("mb")
	messages, _ := atlas.Lookup("messages")
	if messages.AliasOf != "mb" {
		t.Errorf("messages AliasOf = %q, want mb", messages.AliasOf)
	}
	if !slices.Equal(messages.Caps, mb.Caps) || !slices.Equal(messages.Effects, mb.Effects) {
		t.Errorf("messages metadata must match mb: messages caps/effects %v/%v, mb %v/%v",
			messages.Caps, messages.Effects, mb.Caps, mb.Effects)
	}
}

func TestHybridPingSecurityMetadata(t *testing.T) {
	e, ok := atlas.Lookup("ping")
	if !ok {
		t.Fatal("ping front door is absent from atlas")
	}
	for _, effect := range []string{atlas.EffRead, atlas.EffWrite, atlas.EffPersist, atlas.EffNet, atlas.EffExec} {
		if !slices.Contains(e.Effects, effect) {
			t.Errorf("ping effects = %v, missing %q", e.Effects, effect)
		}
	}
	if !slices.Contains(e.Caps, atlas.CapSpawnsProcesses) {
		t.Errorf("ping caps = %v, missing %q for the system-ICMP branch", e.Caps, atlas.CapSpawnsProcesses)
	}
}

func TestMeetSecurityMetadataIncludesDurableConversationState(t *testing.T) {
	e, ok := atlas.Lookup("meet")
	if !ok {
		t.Fatal("meet is absent from atlas")
	}
	for _, effect := range []string{
		atlas.EffRead, atlas.EffWrite, atlas.EffPersist,
		atlas.EffNet, atlas.EffExec, atlas.EffSpend,
	} {
		if !slices.Contains(e.Effects, effect) {
			t.Errorf("meet effects = %v, missing %q", e.Effects, effect)
		}
	}
	if e.Web == nil || e.Web.Mount != "meet" || !slices.Equal(e.Web.Start, []string{"meet", "serve"}) {
		t.Errorf("meet web surface = %+v, want canonical meet owner and start command", e.Web)
	}
}

func TestMeetAndSprintOwnTheirWebSurfaces(t *testing.T) {
	sprint, ok := atlas.Lookup("sprint")
	if !ok || sprint.Web == nil || sprint.Web.Mount != "sprint" {
		t.Fatalf("sprint web surface = %+v, want canonical sprint owner", sprint.Web)
	}
	for _, retired := range []string{"board", "relay"} {
		if _, ok := atlas.Lookup(retired); ok {
			t.Errorf("retired verb %q remains in the atlas", retired)
		}
	}
}

// These POSIX communication applets have deliberately local implementations.
// POSIX owns their interface; this assertion only pins the transport boundary.
func TestPOSIXLocalCommunicationHasNoFallbackEffects(t *testing.T) {
	for _, name := range []string{"mail", "mailx", "talk", "write", "mesg"} {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Errorf("%s is absent from atlas", name)
			continue
		}
		for _, effect := range []string{atlas.EffNet, atlas.EffExec, atlas.EffRemote} {
			if slices.Contains(e.Effects, effect) {
				t.Errorf("%s effects = %v, local POSIX implementation must not declare %q", name, e.Effects, effect)
			}
		}
	}
}

func TestIdiomsReferenceKnownCommands(t *testing.T) {
	// Shell builtins are the embedding shell's to contribute; idioms may
	// name these few without an atlas entry.
	builtinOK := map[string]bool{"cd": true, "trap": true}
	tiers := sliceSet(atlas.Tiers())
	seen := map[string]bool{}
	for _, id := range atlas.Idioms() {
		if id.ID == "" || id.Pattern == "" || id.Note == "" {
			t.Errorf("idiom %+v: id/pattern/note are required", id)
		}
		if seen[id.ID] {
			t.Errorf("duplicate idiom id %q", id.ID)
		}
		seen[id.ID] = true
		if !tiers[id.Tier] {
			t.Errorf("idiom %s: tier %q not in vocabulary", id.ID, id.Tier)
		}
		if len(id.Commands) == 0 {
			t.Errorf("idiom %s: empty commands", id.ID)
		}
		for _, c := range id.Commands {
			if _, ok := atlas.Lookup(c); !ok && !builtinOK[c] {
				t.Errorf("idiom %s: command %q not in atlas", id.ID, c)
			}
		}
	}
}

func TestRegistryEntryDerivation(t *testing.T) {
	e := atlas.RegistryEntry(6)
	if e.Tier != atlas.TierCloud || e.Group != atlas.GroupClusterCloud ||
		e.Subclass != atlas.SubclassManagedExternal {
		t.Errorf("RegistryEntry(6) = %+v, want cloud/cluster-cloud/managed-external", e)
	}
	// A cloud/cluster CLI acts on another host — remote; a tier-2 local tool
	// (ripgrep) is downloaded and run but stays on this box — not remote.
	if !sliceSet(e.Effects)[atlas.EffRemote] {
		t.Errorf("RegistryEntry(6) effects %v missing %q", e.Effects, atlas.EffRemote)
	}
	if sliceSet(atlas.RegistryEntry(2).Effects)[atlas.EffRemote] {
		t.Errorf("RegistryEntry(2) (local tool) must not be %q: %v", atlas.EffRemote, atlas.RegistryEntry(2).Effects)
	}
	if got := atlas.TierName(5); got != atlas.TierCluster {
		t.Errorf("TierName(5) = %q, want cluster", got)
	}
	if got := atlas.TierName(0); got != atlas.TierUserland {
		t.Errorf("TierName(0) = %q, want userland (default)", got)
	}
}

func TestAtlasWhyMetadataPins(t *testing.T) {
	e, ok := atlas.Lookup("why")
	if !ok {
		t.Fatal("expected why to be defined in atlas")
	}

	// Pin managed-external subclass
	if e.Subclass != atlas.SubclassManagedExternal {
		t.Errorf("why subclass = %q, want %q", e.Subclass, atlas.SubclassManagedExternal)
	}

	// Pin cache / self-provisioning / spawns-processes capabilities
	caps := sliceSet(e.Caps)
	for _, c := range []string{atlas.CapCached, atlas.CapSelfProvisioning, atlas.CapSpawnsProcesses} {
		if !caps[c] {
			t.Errorf("why caps missing capability %q", c)
		}
	}

	// Pin no CapReadOnly
	if caps[atlas.CapReadOnly] {
		t.Error("why must not have CapReadOnly")
	}

	// Pin read + exec + write effects
	effs := sliceSet(e.Effects)
	for _, ef := range []string{atlas.EffRead, atlas.EffExec, atlas.EffWrite} {
		if !effs[ef] {
			t.Errorf("why effects missing effect %q", ef)
		}
	}
}
