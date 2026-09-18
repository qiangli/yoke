// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/skills"
)

// isolateStores points every store an underlying registry might open at a
// throwaway directory. A test that reads the operator's real fleet or skill
// ring is testing the host, not the code — and one that writes it is a defect.
func isolateStores(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BASHY_HOME", "BASHY_FLEET_DIR", "BASHY_SKILLS_DIR", "BASHY_TOOLS_DIR", "BASHY_MODELS_DIR", "BASHY_AGENTS_DIR"} {
		t.Setenv(k, t.TempDir())
	}
}

// skillRows builds a catalog over an in-memory embedded ring covering every
// contract a skill can carry: a valid face (exact and judge), prose check-*
// bindings, and nothing at all.
func skillRows(t *testing.T) []SkillRow {
	t.Helper()
	embedded := fstest.MapFS{
		"go-health/SKILL.md":    {Data: []byte("---\nname: go-health\ndescription: build then test\n---\nbody\n")},
		"go-health/skill.dhnt":  {Data: []byte("sokilili gosana efefecato reada fini enisure builida fini sotepo wana reada fini fini\n")},
		"triage/SKILL.md":       {Data: []byte("---\nname: triage\ndescription: judge what to do\n---\nbody\n")},
		"triage/skill.dhnt":     {Data: []byte("sokilili torayagi efefecato reada neto fini enisure gereeni fini sotepo wana reada latitude judage fini fini\n")},
		"prose-checks/SKILL.md": {Data: []byte("---\nname: prose-checks\ndescription: prose with a bound check\nmetadata:\n  check-tests: go test ./...\n---\nbody\n")},
		"plain/SKILL.md":        {Data: []byte("---\nname: plain\ndescription: prose only\n---\nbody\n")},
		"broken/SKILL.md":       {Data: []byte("---\nname: broken\ndescription: a face that does not parse\n---\nbody\n")},
		"broken/skill.dhnt":     {Data: []byte("this is not dhnt\n")},
	}
	cat := &skills.Catalog{Sources: []skills.Source{skills.EmbedSource(embedded, skills.RingEmbedded)}}
	rows, err := cat.List(nil) // no skill gates on a probe, so none is consulted
	if err != nil {
		t.Fatal(err)
	}
	out := make([]SkillRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, skillRowOf(r.Skill))
	}
	return out
}

// skillRowOf is the conversion an embedding shell writes at its call site:
// lexicon takes the executor-free view, never the skills.Skill itself.
func skillRowOf(sk skills.Skill) SkillRow {
	row := SkillRow{Name: sk.Name, Description: sk.Description, Meta: sk.Meta}
	if sk.Dhnt.Valid() {
		row.FaceValid = true
		row.Identity = sk.Dhnt.Identity
		row.EffectCap = sk.Dhnt.EffectCap
		row.HasJudgeStep = sk.Dhnt.HasJudgeStep
	}
	return row
}

func facetOf(t *testing.T, s *Store, term string) *ActionFacet {
	t.Helper()
	c, ok := s.Resolve(term)
	if !ok {
		t.Fatalf("%q did not resolve", term)
	}
	if c.Action == nil {
		t.Fatalf("%q (%s) has no action facet", term, c.Kind)
	}
	return c.Action
}

// A verb's facet is the atlas record, projected: exact, deterministic, the
// atlas effects carried verbatim beside their dhnt-6 projection.
func TestAction_CommandFacet(t *testing.T) {
	s := store(t)
	for _, name := range []string{"handoff", "kb"} {
		e, ok := atlas.Lookup(name)
		if !ok {
			t.Fatalf("atlas has no %q", name)
		}
		got := facetOf(t, s, name)
		want := &ActionFacet{
			Kind: ActionCommand, Identity: "verb:" + name,
			Contract: ContractNone, Latitude: LatitudeExact, Authority: AuthorityDeterministic,
			EffectsDeclared: atlas.ProjectEffects(e.Effects), AtlasEffects: e.Effects,
			Executor: ExecutorVerb, Envelope: EnvelopeRun, Scope: ScopeGeneric,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
		if len(got.AtlasEffects) == 0 {
			t.Errorf("%s: atlas effects empty — every atlas entry declares at least one", name)
		}
	}
	// An alias is a term for the target's concept, so it carries the target's
	// facet, not one of its own.
	if got := facetOf(t, s, "invoke"); got.Identity != "verb:chat" {
		t.Errorf("alias invoke → %q, want the target verb:chat", got.Identity)
	}
}

// The pure-Go userland is exact/deterministic like a verb, but the executor is
// the in-process tool, and the effects still come off the atlas record.
func TestAction_StandardToolFacet(t *testing.T) {
	s := store(t)
	s.AddStandardTools([]string{"rm", "cat", "not-in-the-atlas"}, Overlay{})
	rm := facetOf(t, s, "rm")
	if rm.Executor != ExecutorCoreutils || rm.Identity != "verb:rm" || rm.Envelope != EnvelopeRun {
		t.Errorf("rm facet = %+v", rm)
	}
	if !contains(rm.AtlasEffects, atlas.EffDestroy) || !contains(rm.EffectsDeclared, "destroy") {
		t.Errorf("rm must project destroy on both sides: %+v", rm)
	}
	if !contains(facetOf(t, s, "cat").EffectsDeclared, "read") {
		t.Error("cat must declare read")
	}
	// A name the atlas has no record for gets NO facet — an empty one would
	// read as "declares no effects", which is a claim nobody made.
	if c, _ := s.Resolve("not-in-the-atlas"); c == nil || c.Action != nil {
		t.Errorf("unknown standard tool: want concept without a facet, got %+v", c)
	}
}

// A skill's contract is the strongest thing it actually carries, and one judge
// step makes the whole run agentic.
func TestAction_SkillFacets(t *testing.T) {
	isolateStores(t)
	s := store(t)
	s.AddSkills(skillRows(t), Overlay{})

	health := facetOf(t, s, "go-health")
	if health.Kind != ActionSkill || health.Contract != ContractDhnt || !strings.HasPrefix(health.Identity, "h") {
		t.Errorf("go-health: %+v", health)
	}
	if health.Latitude != LatitudeExact || health.Authority != AuthorityDeterministic {
		t.Errorf("go-health has no judge step, must be exact/deterministic: %+v", health)
	}
	if !reflect.DeepEqual(health.EffectsDeclared, []string{"read"}) || health.AtlasEffects != nil {
		t.Errorf("go-health effects: declared=%v atlas=%v", health.EffectsDeclared, health.AtlasEffects)
	}
	if health.Executor != ExecutorDhnt || health.Envelope != EnvelopeAttest || health.Scope != ScopeGeneric {
		t.Errorf("go-health executor/envelope/scope: %+v", health)
	}

	triage := facetOf(t, s, "triage")
	if triage.Latitude != LatitudeJudge || triage.Authority != AuthorityAgentic || triage.Contract != ContractDhnt {
		t.Errorf("triage has a judge step, must be judge/agentic: %+v", triage)
	}
	if !reflect.DeepEqual(triage.EffectsDeclared, []string{"read", "net"}) {
		t.Errorf("triage effect cap: %v", triage.EffectsDeclared)
	}

	prose := facetOf(t, s, "prose-checks")
	if prose.Contract != ContractMetadata || prose.Identity != "" || prose.EffectsDeclared != nil {
		t.Errorf("prose-checks: %+v", prose)
	}
	if plain := facetOf(t, s, "plain"); plain.Contract != ContractNone || plain.Identity != "" {
		t.Errorf("plain: %+v", plain)
	}
	// An invalid face is not a contract, and it must not masquerade as one.
	if broken := facetOf(t, s, "broken"); broken.Contract != ContractNone || broken.Identity != "" {
		t.Errorf("broken face: %+v", broken)
	}
	if c, _ := s.Resolve("skill:triage"); c == nil || c.Kind != KindSkill || c.Use != "bashy skill run triage" {
		t.Errorf("skill concept: %+v", c)
	}
}

// A binding is an agent: judge, agentic, host-scoped, launched through its
// tool. The TOOL concept the human names is the executor and gets no facet.
func TestAction_BindingFacet_ToolHasNone(t *testing.T) {
	isolateStores(t)
	fleettest.Ring(t) // the agent under test comes from the test ring
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutLocalStore(), fleet.WithoutCloudOverlay())
	s := Build(cat, nil, "test-host", Overlay{})

	a, ok := cat.Agent("codex-gpt-5.5")
	if !ok {
		t.Fatal("baseline has no codex-gpt-5.5")
	}
	got := facetOf(t, s, "agent:"+a.Name)
	want := &ActionFacet{
		Kind: ActionAgent, Identity: a.MatrixKey(),
		Contract: ContractNone, Latitude: LatitudeJudge, Authority: AuthorityAgentic,
		Executor: "agentlaunch:" + a.Tool, Envelope: EnvelopeChat, Scope: ScopeHost,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
	if got.Identity != "codex:gpt-5.5" || got.Executor != "agentlaunch:codex" {
		t.Errorf("identity/executor: %+v", got)
	}

	tool, ok := s.Resolve("tool:codex")
	if !ok || tool.Kind != KindTool {
		t.Fatalf("tool:codex resolved to %+v", tool)
	}
	if tool.Action != nil {
		t.Errorf("a tool is the executor of an action, not an action: %+v", tool.Action)
	}
}

// The ratchet. `action` is a closed vocabulary elsewhere (activity, llmbudget,
// audit, steward, browser, meet): in anything this package emits the word
// appears ONLY as a nested object under a concept — never top-level, never a
// string. And Location/Host stay out of the shareable block.
func TestAction_NeverEmitsBareActionKey(t *testing.T) {
	isolateStores(t)
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutLocalStore(), fleet.WithoutCloudOverlay())
	s := Build(cat, nil, "hostname-under-test", Overlay{})
	s.AddStandardTools([]string{"rm"}, Overlay{})
	s.AddSkills(skillRows(t), Overlay{})
	s.AddSystem(SystemInventory{
		Commands:     []string{"outpost"},
		CommandPaths: map[string]string{"outpost": "/Users/alice/bin/outpost"},
	}, Overlay{})

	var facets int
	for _, c := range s.Concepts {
		if c.Action != nil {
			facets++
		}
	}
	if facets == 0 {
		t.Fatal("no facets projected — the ratchet would be vacuous")
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	checkActionKeys(t, "json", doc, true)

	y, err := yaml.Marshal(s.Concepts)
	if err != nil {
		t.Fatal(err)
	}
	var ydoc any
	if err := yaml.Unmarshal(y, &ydoc); err != nil {
		t.Fatal(err)
	}
	checkActionKeys(t, "yaml", ydoc, true)

	got := s.EmitAgentsMD("demo")
	for _, leak := range []string{"hostname-under-test", "/Users/alice", "alice"} {
		if strings.Contains(got, leak) {
			t.Errorf("emit leaked %q into a shareable artifact", leak)
		}
	}
}

// checkActionKeys walks a decoded document. At the top level `action` must be
// absent; below it, a key named `action` must hold an object.
func checkActionKeys(t *testing.T, format string, v any, top bool) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "action" {
				if top {
					t.Errorf("%s: top-level %q key", format, k)
				}
				if _, isObj := val.(map[string]any); !isObj {
					t.Errorf("%s: %q holds %T, must be a nested object", format, k, val)
				}
			}
			checkActionKeys(t, format, val, false)
		}
	case []any:
		for _, e := range x {
			checkActionKeys(t, format, e, false)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// A registered command (`bashy commands add`) is projected like a shipped
// verb: one concept, aliases as terms, and an exact/deterministic command
// facet whose effects are the AUTHOR's declaration and whose executor says
// it came from the ring.
func TestAction_RegisteredCommandFacet(t *testing.T) {
	isolateStores(t)
	fleettest.Ring(t)
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	if err := cat.SaveCommand(fleet.Command{
		Name: "gl", Aliases: []string{"glog"}, Synopsis: "compact log",
		Script: "git log --oneline", Effects: []string{atlas.EffRead, atlas.EffExec},
	}); err != nil {
		t.Fatal(err)
	}
	s := Build(cat, nil, "test-host", Overlay{})
	got := facetOf(t, s, "verb:gl")
	want := &ActionFacet{
		Kind: ActionCommand, Identity: "verb:gl",
		Contract: ContractNone, Latitude: LatitudeExact, Authority: AuthorityDeterministic,
		EffectsDeclared: atlas.ProjectEffects([]string{atlas.EffExec, atlas.EffRead}),
		AtlasEffects:    []string{atlas.EffExec, atlas.EffRead},
		Executor:        ExecutorRegistered, Envelope: EnvelopeRun, Scope: ScopeGeneric,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
	c, ok := s.Resolve("glog")
	if !ok || c.ID != "verb:gl" || c.Source != "commands-registry" {
		t.Errorf("alias glog resolved to %+v", c)
	}
}
