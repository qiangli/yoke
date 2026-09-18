package lexicon

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s := &Store{byTerm: map[string]int{}, byTermAll: map[string][]int{}}
	s.AddStandardTools([]string{"ls", "grep"}, Overlay{})
	s.AddSystem(SystemInventory{
		EnvVars:         []string{"WEAVE_AGENT"},
		StandardEnvVars: []string{"HOME", "PATH"},
		Commands:        []string{"outpost", "host-a"},
		PathSegments:    []string{"dhnt", "host-a"}, // host-a is BOTH — the ambiguous case
	}, Overlay{})
	return s
}

// The gap that made `define` wrong on its own terms: a verb called define that
// answers "unknown" for `ls`. Standard tools are not jargon, but they ARE
// applicable to this system, and an agent asking deserves the right pointer.
func TestDefine_AnswersForStandardTools(t *testing.T) {
	d := testStore(t).Define("ls")
	if !d.Found {
		t.Fatalf("ls was not found: %+v", d)
	}
	if d.Concept.Kind != KindStandardTool {
		t.Errorf("kind = %s, want %s", d.Concept.Kind, KindStandardTool)
	}
	if !strings.Contains(d.Concept.ScopeNote, "not local jargon") {
		t.Errorf("the scope note should distinguish standard from local: %q", d.Concept.ScopeNote)
	}
}

// The same gap, one layer over: `define PATH` answered "unknown here" from a
// fully populated host, because the enumerator subtracts the standard set to
// keep the GLOSSARY free of noise and the RESOLVER inherited that subtraction.
// The most standard name on the machine is also the one most often asked about.
func TestDefine_AnswersForStandardEnvVars(t *testing.T) {
	for _, name := range []string{"PATH", "HOME"} {
		d := testStore(t).Define(name)
		if !d.Found {
			t.Fatalf("%s was not found: %+v", name, d)
		}
		if d.Concept.Kind != KindStandardEnvVar {
			t.Errorf("%s: kind = %s, want %s", name, d.Concept.Kind, KindStandardEnvVar)
		}
		if !strings.Contains(d.Concept.ScopeNote, "not local jargon") {
			t.Errorf("%s: the scope note should distinguish standard from local: %q",
				name, d.Concept.ScopeNote)
		}
	}
	// The local bucket keeps its own kind — the two must stay distinguishable,
	// because "this fleet sets it" and "every host has it" are different facts.
	if d := testStore(t).Define("WEAVE_AGENT"); d.Concept.Kind != KindEnvVar {
		t.Errorf("local env var kind = %s, want %s", d.Concept.Kind, KindEnvVar)
	}
}

// The SEAM, on the live machine. The two halves above are injected, and both
// passed while `bashy define PATH` on a fully populated host still answered
// "unknown here" — because the wiring between them dropped the set. A test that
// only ever sees synthetic input cannot see that, so this one runs the real
// enumerator, including the host scrubber, against the one environment variable
// every machine has.
func TestEnumerateHost_FeedsDefineOnThisMachine(t *testing.T) {
	if os.Getenv("PATH") == "" {
		t.Skip("no PATH in this environment; nothing to enumerate")
	}
	s := &Store{byTerm: map[string]int{}, byTermAll: map[string][]int{}}
	s.AddSystem(EnumerateHost(nil, nil), Overlay{})

	d := s.Define("PATH")
	if !d.Found {
		t.Fatalf("PATH is set on this host but define does not know it: %+v", d)
	}
	if d.Concept.Kind != KindStandardEnvVar {
		t.Errorf("kind = %s, want %s", d.Concept.Kind, KindStandardEnvVar)
	}
}

// One string genuinely IS several things on a real host. Reporting only the
// first is how a caller confidently acts in the wrong world.
func TestDefine_ReportsEveryReading(t *testing.T) {
	d := testStore(t).Define("host-a")
	if !d.Found {
		t.Fatal("host-a was not found")
	}
	if len(d.Concepts) != 2 {
		t.Fatalf("got %d readings, want 2 (a command AND a path segment): %+v", len(d.Concepts), d.Concepts)
	}
	var kinds []string
	for _, c := range d.Concepts {
		kinds = append(kinds, string(c.Kind))
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"command", "path-segment"}) {
		t.Errorf("kinds = %v", kinds)
	}
	// The primary reading is the first, so a caller wanting one answer can take
	// the head without losing the fact that there were others.
	if d.Concept != d.Concepts[0] {
		t.Error("Concept is not the head of Concepts")
	}
}

// An unambiguous term still reports exactly one reading.
func TestDefine_SingleReading(t *testing.T) {
	d := testStore(t).Define("WEAVE_AGENT")
	if len(d.Concepts) != 1 {
		t.Errorf("got %d readings, want 1", len(d.Concepts))
	}
}

// A filter NARROWS; it never invents.
func TestDefineKinds_Filter(t *testing.T) {
	s := testStore(t)

	only := s.DefineKinds("host-a", []Kind{KindCommand})
	if len(only.Concepts) != 1 || only.Concepts[0].Kind != KindCommand {
		t.Fatalf("filtered result = %+v, want exactly the command reading", only.Concepts)
	}

	// Asking for a kind the term does not have is "not known AS THAT" — a
	// different and more useful statement than "not known".
	none := s.DefineKinds("host-a", []Kind{KindVerb})
	if none.Found {
		t.Error("a filter invented a reading the term does not have")
	}
	if !strings.Contains(none.Advice, "not known as verb") {
		t.Errorf("advice should name the filter that failed: %q", none.Advice)
	}
	if !strings.Contains(none.Advice, "Drop the filter") {
		t.Errorf("advice should offer the way forward: %q", none.Advice)
	}
}

// A shape classification is about the STRING, so a namespace filter must not
// produce one — answering "that is an IP" when the caller asked about commands
// answers a question nobody asked.
func TestDefineKinds_FilterSuppressesShapeAnswer(t *testing.T) {
	s := testStore(t)
	if d := s.Define("10.1.2.3"); d.Classification == "" {
		t.Error("unfiltered lookup should classify an address by shape")
	}
	if d := s.DefineKinds("10.1.2.3", []Kind{KindCommand}); d.Classification != "" {
		t.Errorf("a kind filter still produced a shape answer: %q", d.Classification)
	}
}

// The credential check runs before ANY lookup, and the term never comes back.
func TestDefine_CredentialNeverEchoed(t *testing.T) {
	const key = "sk-proj-Ab3xK9mQ7zR2vN8pL4wT6yH1jF5dG0sE"
	d := testStore(t).Define(key)

	if !d.Sensitive {
		t.Fatal("a vendor-prefixed key was not flagged sensitive")
	}
	if d.Term != "" {
		t.Errorf("the term was echoed back: %q", d.Term)
	}
	if d.Found || d.Concept != nil || len(d.Concepts) != 0 {
		t.Error("a sensitive term was looked up; the check must precede resolution")
	}
	if !strings.Contains(d.Advice, "rotate") {
		t.Errorf("advice should say what to do: %q", d.Advice)
	}
}

// Git shas are ubiquitous; calling every one a secret trains people to ignore
// the warning that matters.
func TestDefine_HexDigestIsNotACredential(t *testing.T) {
	d := testStore(t).Define("7420470a1b2c3d4e5f60718293a4b5c6d7e8f900")
	if d.Sensitive {
		t.Error("a hex digest was flagged as a credential")
	}
	if !strings.Contains(d.Classification, "hex digest") {
		t.Errorf("classification = %q", d.Classification)
	}
}

func TestDefine_Unknown(t *testing.T) {
	d := testStore(t).Define("frobnicate")
	if d.Found || d.Sensitive || d.Classification != "" {
		t.Errorf("an unknown word produced an answer: %+v", d)
	}
	if d.Advice == "" {
		t.Error("an unknown word should still say where to look")
	}
}

func TestDefine_Empty(t *testing.T) {
	if d := (&Store{}).Define("   "); d.Found || d.Advice == "" {
		t.Errorf("empty input = %+v", d)
	}
}

// Kinds is derived from what is projected, so a --kind filter's vocabulary
// cannot drift from what the host can actually answer for.
func TestStore_Kinds(t *testing.T) {
	got := testStore(t).Kinds()
	for _, want := range []string{"command", "env-var", "path-segment", "standard-tool", "standard-env-var"} {
		if !slices.Contains(got, want) {
			t.Errorf("Kinds() = %v, missing %q", got, want)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("Kinds() is not sorted: %v", got)
	}
}

// `define` must never gain a subcommand.
//
// Its argument is an arbitrary user token, so every subcommand name permanently
// removes a word from the definable vocabulary: mount `study` and
// `bashy define study` stops meaning "what is the word study". The failure is
// invisible until somebody asks about that exact word and gets a help screen
// instead of an answer — which is why this is a build-failing ratchet rather
// than a note in a doc.
//
// Actions belong under `lexicon`, whose arguments are a closed set.
func TestDefineCmd_HasNoSubcommands(t *testing.T) {
	cmd := NewDefineCmd()
	if subs := cmd.Commands(); len(subs) != 0 {
		names := make([]string, 0, len(subs))
		for _, c := range subs {
			names = append(names, c.Name())
		}
		t.Fatalf("define has subcommands %v — each one steals that word from the vocabulary; "+
			"put the action under `lexicon` instead", names)
	}
}

// `define study` must keep meaning "what is the word study".
//
// The companion to the ratchet above, asserted through the command rather than
// its shape: a subcommand would make cobra intercept the token before Args ever
// ran, and the caller would get a help screen instead of an answer. Asserting
// the shape alone would not catch a `study` mounted on a PARENT that passes
// through.
func TestDefineCmd_TreatsAVerbNameAsAWord(t *testing.T) {
	cmd := NewDefineCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"study"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("define study: %v", err)
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Fatalf("`define study` printed a help screen — the word is no longer definable:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "study") {
		t.Errorf("the answer does not mention the term asked about:\n%s", out.String())
	}
}

// The way OUT of "unknown here" has to work.
//
// The unknown-term advice names `define --list-kinds` as the place to look, and
// under ExactArgs(1) that command answered "accepts 1 arg(s), received 0" — so
// the honest "I don't know" pointed at a dead end, which is a worse failure
// than the unknown answer it was trying to soften.
func TestDefineCmd_ListKindsNeedsNoTerm(t *testing.T) {
	cmd := NewDefineCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--list-kinds"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("define --list-kinds: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), string(KindStandardTool)) {
		t.Errorf("--list-kinds did not name the namespaces:\n%s", out.String())
	}
}

// ...but a bare `define` with no term is still an error, said plainly. The
// relaxed arity is for the flag, not a licence to answer nothing.
func TestDefineCmd_BareInvocationIsAnError(t *testing.T) {
	cmd := NewDefineCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Fatalf("bare `define` succeeded:\n%s", out.String())
	}
}

// The RENDERED answer is where a credential would actually leak — into a
// terminal, a scrollback buffer, an agent transcript. Definition.Term being
// empty is the mechanism; this is the property, asserted on both output modes
// because --json is the one an agent pipes somewhere permanent.
func TestDefineCmd_CredentialIsNotRenderedInEitherMode(t *testing.T) {
	const key = "sk-proj-Ab3xK9mQ7zR2vN8pL4wT6yH1jF5dG0sE"
	for _, args := range [][]string{{key}, {key, "--json"}} {
		cmd := NewDefineCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("define %v: %v", args, err)
		}
		if strings.Contains(out.String(), key) {
			t.Fatalf("define %v echoed the credential:\n%s", args, out.String())
		}
		// Silence would be worse than useless here: "that is a credential" is
		// the whole value of the answer.
		if !strings.Contains(out.String(), "credential") {
			t.Errorf("define %v did not classify the term:\n%s", args, out.String())
		}
	}
}

// A wired SkillSource makes `define <skill>` answer with the skill's facet;
// unwired, the host says "unknown here" rather than guessing.
func TestDefine_SkillSourceSeam(t *testing.T) {
	defer func(prev func() []SkillRow) { SkillSource = prev }(SkillSource)
	SkillSource = func() []SkillRow {
		return []SkillRow{{Name: "go-health", Description: "build then test", FaceValid: true, Identity: "h" + strings.Repeat("0", 64), EffectCap: []string{"read"}}}
	}
	cmd := NewDefineCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"go-health"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "runs: skill contract=dhnt latitude=exact authority=deterministic effects=read via dhnt (attest-jsonl)") {
		t.Fatalf("define go-health did not print the skill facet:\n%s", out.String())
	}
	SkillSource = nil
	out.Reset()
	cmd = NewDefineCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"go-health"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown here") {
		t.Fatalf("unwired SkillSource still answered:\n%s", out.String())
	}
}
