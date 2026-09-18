package lexicon

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/redact"
)

// THE KEYS-ONLY RULE. An env value may be a credential; the name never is. This
// is the property the whole file is built around, so it is asserted directly
// rather than inferred from the type.
func TestEnumerate_EnvValuesNeverSurvive(t *testing.T) {
	inv := Enumerate(EnumOptions{
		Environ: []string{
			"WEAVE_AGENT=elif",
			"ANTHROPIC_API_KEY=sk-proj-abc123secretvalue",
			"BASHY_SKILLS_DIR=/somewhere",
		},
	})

	if !slices.Contains(inv.EnvVars, "ANTHROPIC_API_KEY") {
		t.Error("the KEY should be collected — it is a legitimate term")
	}
	for _, term := range inv.EnvVars {
		if term == "sk-proj-abc123secretvalue" {
			t.Fatal("an env VALUE was collected as a term")
		}
		for _, leak := range []string{"sk-proj", "secretvalue", "/somewhere", "elif"} {
			if term == leak {
				t.Fatalf("a value fragment %q was collected", leak)
			}
		}
	}
}

// Standard env vars are the same everywhere, so they carry no local meaning.
func TestEnumerate_SubtractsStandardEnvVars(t *testing.T) {
	inv := Enumerate(EnumOptions{
		Environ: []string{
			"PATH=/usr/bin", "HOME=/home/x", "SHELL=/bin/sh", "LANG=en_US.UTF-8",
			"LC_ALL=C", "TMPDIR=/tmp", "XDG_CONFIG_HOME=/c",
			"WEAVE_AGENT=elif", "MATRIX_TOKEN=x", "BASHY_HINTS=off",
		},
	})

	want := []string{"BASHY_HINTS", "MATRIX_TOKEN", "WEAVE_AGENT"}
	if !slices.Equal(inv.EnvVars, want) {
		t.Errorf("EnvVars = %v, want only the non-standard ones %v", inv.EnvVars, want)
	}
}

// Subtracted from the GLOSSARY, kept for the RESOLVER. The two want different
// sets and the split is the whole point: emit teaches jargon, define answers
// questions — and "what is PATH" is a question a host must be able to answer.
func TestEnumerate_StandardEnvVarsAreKeptSeparately(t *testing.T) {
	inv := Enumerate(EnumOptions{
		Environ: []string{
			"PATH=/usr/bin", "HOME=/home/x", "LC_ALL=C", "WEAVE_AGENT=elif",
		},
	})

	wantStd := []string{"HOME", "LC_ALL", "PATH"}
	if !slices.Equal(inv.StandardEnvVars, wantStd) {
		t.Errorf("StandardEnvVars = %v, want %v", inv.StandardEnvVars, wantStd)
	}
	if slices.Contains(inv.EnvVars, "PATH") {
		t.Error("a standard key leaked into the local-jargon bucket")
	}
	// Keys only, here too: the bucket a name lands in must not change the rule.
	for _, term := range inv.StandardEnvVars {
		for _, leak := range []string{"/usr/bin", "/home/x", "C"} {
			if term == leak {
				t.Fatalf("an env VALUE %q was collected as a standard term", leak)
			}
		}
	}
}

// A standard key that this host does NOT set is not a term here. Enumeration is
// what makes every answer real by construction; emitting the whole standard set
// unconditionally would be a hard-coded list wearing an inventory's name.
func TestEnumerate_UnsetStandardEnvVarsAreNotCollected(t *testing.T) {
	inv := Enumerate(EnumOptions{Environ: []string{"PATH=/usr/bin"}})
	if slices.Contains(inv.StandardEnvVars, "HOME") {
		t.Error("HOME was collected although this environment does not set it")
	}
}

// Standard userland commands are subtracted; what remains is local.
func TestEnumerate_SubtractsKnownCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit semantics differ on windows; covered by the unit filters")
	}
	dir := t.TempDir()
	for _, name := range []string{"ls", "grep", "outpost", "weave", "loom"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A subdirectory is not a command.
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	inv := Enumerate(EnumOptions{
		PathDirs:      []string{dir},
		KnownCommands: []string{"ls", "grep"},
	})

	want := []string{"loom", "outpost", "weave"}
	if !slices.Equal(inv.Commands, want) {
		t.Errorf("Commands = %v, want %v", inv.Commands, want)
	}
}

// An unreadable PATH entry is ordinary and must not fail enumeration.
func TestEnumerate_ToleratesBadPathDirs(t *testing.T) {
	inv := Enumerate(EnumOptions{PathDirs: []string{"/definitely/not/here", ""}})
	if len(inv.Commands) != 0 {
		t.Errorf("Commands = %v, want none", inv.Commands)
	}
}

// Versioned duplicates denote the same concept as their base name and would
// inflate the term set without adding meaning.
func TestEnumerate_SkipsVersionedDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit semantics differ on windows")
	}
	dir := t.TempDir()
	for _, name := range []string{"python3.12", "gcc-14", "outpost"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inv := Enumerate(EnumOptions{PathDirs: []string{dir}})
	if slices.Contains(inv.Commands, "python3.12") {
		t.Error("a versioned duplicate was collected")
	}
	if !slices.Contains(inv.Commands, "outpost") {
		t.Error("a real local command was dropped")
	}
}

func TestEnumerate_PathSegments(t *testing.T) {
	inv := Enumerate(EnumOptions{
		Roots: []string{"/opt/projects/poc/dhnt/coreutils/pkg/lexicon"},
	})

	// Generic segments carry the same meaning everywhere and are dropped.
	for _, generic := range []string{"opt", "projects", "pkg"} {
		if slices.Contains(inv.PathSegments, generic) {
			t.Errorf("generic segment %q was collected", generic)
		}
	}
	// Local ones are what we are after.
	for _, local := range []string{"dhnt", "coreutils", "lexicon"} {
		if !slices.Contains(inv.PathSegments, local) {
			t.Errorf("local segment %q was not collected (got %v)", local, inv.PathSegments)
		}
	}
}

// THE IDENTITY FILTER. A home directory's own path contains a username, so
// without this the path segments of any real machine include the operator's
// login. A term that IS someone's name is a fact about a machine, not
// vocabulary.
func TestEnumerate_DropsIdentifyingTerms(t *testing.T) {
	scrub := redact.New(redact.WithUser("alice"), redact.WithHost("workshop"))

	inv := Enumerate(EnumOptions{
		Roots:    []string{"/home/alice/projects/dhnt"},
		Environ:  []string{"WORKSHOP_MODE=1", "PROJECT=dhnt"},
		Scrubber: scrub,
	})

	if slices.Contains(inv.PathSegments, "alice") {
		t.Errorf("a username was collected as a term: %v", inv.PathSegments)
	}
	if !slices.Contains(inv.PathSegments, "dhnt") {
		t.Errorf("the identity filter also removed a legitimate term: %v", inv.PathSegments)
	}
}

// Without a scrubber the identity filter is off — acceptable for synthetic test
// input, and asserted so the default is never mistaken for "safe".
func TestEnumerate_NoScrubberMeansNoFilter(t *testing.T) {
	inv := Enumerate(EnumOptions{Roots: []string{"/home/alice/projects/dhnt"}})
	if !slices.Contains(inv.PathSegments, "alice") {
		t.Error("fixture no longer demonstrates that the filter is opt-in")
	}
}

func TestEnumerate_Deduplicates(t *testing.T) {
	inv := Enumerate(EnumOptions{
		Environ: []string{"WEAVE_AGENT=a", "WEAVE_AGENT=b"},
		Roots:   []string{"/x/dhnt/y", "/z/dhnt/w"},
	})
	if len(inv.EnvVars) != 1 {
		t.Errorf("EnvVars = %v, want one entry", inv.EnvVars)
	}
	if n := countOf(inv.PathSegments, "dhnt"); n != 1 {
		t.Errorf("dhnt appears %d times, want 1", n)
	}
}

func TestEnumerate_Empty(t *testing.T) {
	inv := Enumerate(EnumOptions{})
	if len(inv.EnvVars)+len(inv.Commands)+len(inv.PathSegments) != 0 {
		t.Errorf("empty options produced %+v", inv)
	}
}

// Projection into the store, and resolution by term.
func TestAddSystem_ProjectsAndResolves(t *testing.T) {
	s := &Store{byTerm: map[string]int{}}
	s.AddSystem(SystemInventory{
		EnvVars:      []string{"WEAVE_AGENT"},
		Commands:     []string{"outpost"},
		PathSegments: []string{"dhnt"},
	}, Overlay{})

	tests := []struct {
		term string
		kind Kind
	}{
		{"WEAVE_AGENT", KindEnvVar},
		{"outpost", KindCommand},
		{"dhnt", KindPathSegment},
	}
	for _, tc := range tests {
		got, ok := s.Resolve(tc.term)
		if !ok {
			t.Errorf("%q did not resolve", tc.term)
			continue
		}
		if got.Kind != tc.kind {
			t.Errorf("%q kind = %s, want %s", tc.term, got.Kind, tc.kind)
		}
		if got.ScopeNote == "" {
			t.Errorf("%q has no scope note — the precedence rule is the point", tc.term)
		}
		if got.Source == "" {
			t.Errorf("%q has no source; provenance must say which registry it came from", tc.term)
		}
	}
}

func countOf(vals []string, want string) int {
	n := 0
	for _, v := range vals {
		if v == want {
			n++
		}
	}
	return n
}
