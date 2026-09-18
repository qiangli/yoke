package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func writeSkill(t *testing.T, dir, name, frontmatter string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(frontmatter+"\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogListGatingAndShadowing(t *testing.T) {
	embedded := fstest.MapFS{
		"alpha-notes/SKILL.md":    {Data: []byte("---\nname: alpha-notes\ndescription: rung-0 prose, no conditions\n---\nbody\n")},
		"beta-deploy/SKILL.md":    {Data: []byte("---\nname: beta-deploy\ndescription: gated\nmetadata:\n  requires: \"os=linux has=examplectl\"\n---\nbody\n")},
		"gamma-compat/SKILL.md":   {Data: []byte("---\nname: gamma-compat\ndescription: prose compat only\ncompatibility: requires exampled running\n---\nbody\n")},
		"delta-broken/SKILL.md":   {Data: []byte("no frontmatter here\n")},
		"epsilon-dhnt/SKILL.md":   {Data: []byte("---\nname: epsilon-dhnt\ndescription: dual bundle\n---\nbody\n")},
		"epsilon-dhnt/skill.dhnt": {Data: []byte("sokilili epsilon fini\n")},
	}
	local := t.TempDir()
	// Local override of an embedded skill (shadowing) + a local-only one.
	writeSkill(t, local, "alpha-notes", "---\nname: alpha-notes\ndescription: local override\n---")
	writeSkill(t, local, "zeta-local", "---\nname: zeta-local\ndescription: local only\nmetadata:\n  requires: \"os=linux,darwin,windows\"\n---")

	cat := &Catalog{Sources: []Source{EmbedSource(embedded, RingEmbedded), DirSource(local)}}
	ps := testProbes(t,
		map[string]string{"os": "linux", "arch": "arm64"},
		map[string]string{}, // examplectl absent
	)
	rows, err := cat.List(ps)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Listing{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 (%v)", len(rows), names(rows))
	}
	if r := byName["alpha-notes"]; !r.Verdict.Applicable || !r.Shadows || r.Ring != RingLocal || r.Description != "local override" {
		t.Errorf("alpha-notes: %+v", r)
	}
	if r := byName["beta-deploy"]; r.Verdict.Applicable || r.Verdict.Failing != "has=examplectl: absent" {
		t.Errorf("beta-deploy: %+v", r.Verdict)
	}
	if r := byName["gamma-compat"]; !r.Verdict.Applicable || r.Verdict.Unchecked == "" {
		t.Errorf("gamma-compat: %+v", r.Verdict)
	}
	// Malformed frontmatter degrades to dir name, applicable, warned.
	if r := byName["delta-broken"]; !r.Verdict.Applicable || r.Warning == "" {
		t.Errorf("delta-broken: %+v", r)
	}
	if r := byName["epsilon-dhnt"]; !r.HasDhnt {
		t.Errorf("epsilon-dhnt: HasDhnt = false")
	}
	if r := byName["zeta-local"]; !r.Verdict.Applicable || r.Ring != RingLocal {
		t.Errorf("zeta-local: %+v", r)
	}
}

func names(rows []Listing) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func TestCatalogGetPrecedence(t *testing.T) {
	embedded := fstest.MapFS{
		"alpha-notes/SKILL.md": {Data: []byte("---\nname: alpha-notes\ndescription: embedded\n---\nEMBEDDED BODY\n")},
	}
	local := t.TempDir()
	writeSkill(t, local, "alpha-notes", "---\nname: alpha-notes\ndescription: local\n---")

	cat := &Catalog{Sources: []Source{EmbedSource(embedded, RingEmbedded), DirSource(local)}}
	sk, src, ok := cat.Get("alpha-notes")
	if !ok || sk.Ring != RingLocal || src.Ring() != RingLocal {
		t.Fatalf("Get precedence: %+v", sk)
	}
	if _, _, ok := cat.Get("missing"); ok {
		t.Fatal("Get(missing) = ok")
	}
}

func TestSourcePathSafety(t *testing.T) {
	embedded := fstest.MapFS{"alpha-notes/SKILL.md": {Data: []byte("x")}}
	src := EmbedSource(embedded, RingEmbedded)
	for _, bad := range [][2]string{{"../alpha-notes", "SKILL.md"}, {"a/b", "SKILL.md"}, {"alpha-notes", "../../secret"}} {
		if _, ok := src.File(bad[0], bad[1]); ok {
			t.Errorf("File(%q, %q) allowed", bad[0], bad[1])
		}
	}
}

func TestKeyProbes(t *testing.T) {
	req, err := ParseRequires("os=linux has=git go>=1.26")
	if err != nil {
		t.Fatal(err)
	}
	sk := Skill{Requires: &req}
	got := KeyProbes(sk)
	want := []string{"os", "arch", "tool.git", "tool.go"}
	if len(got) != len(want) {
		t.Fatalf("KeyProbes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KeyProbes = %v, want %v", got, want)
		}
	}
	// No requires → coarse {os, arch} only.
	if kp := KeyProbes(Skill{}); len(kp) != 2 {
		t.Fatalf("KeyProbes(no requires) = %v", kp)
	}
}

// A skill may GATE on the clock but must never be KEYED by it: a coordinate that
// advances with the hour files every run under a fresh address, so no skill ever
// accumulates the evidence that election and retirement depend on. The failure is
// silent — the store just fills with singletons — so it is pinned here, at the one
// place a skill's coordinate is minted.
func TestKeyProbes_ExcludesTheClock(t *testing.T) {
	req, err := ParseRequires("os=linux time.attended has=git")
	if err != nil {
		t.Fatal(err)
	}
	sk := Skill{Requires: &req}

	// The gate still sees it — dropping it from the KEY must not drop it from
	// applicability, or a skill needing a human present would run unattended.
	refs := req.ProbeRefs()
	if !slices.Contains(refs, "time.attended") {
		t.Fatalf("ProbeRefs = %v, want the clause preserved for gating", refs)
	}

	got := KeyProbes(sk)
	if slices.Contains(got, "time.attended") {
		t.Errorf("KeyProbes = %v; a clock probe reached the context key", got)
	}
	if want := []string{"os", "arch", "tool.git"}; !slices.Equal(got, want) {
		t.Errorf("KeyProbes = %v, want %v", got, want)
	}

	// Network locality is deliberately still keyable — reachability is a real
	// capability difference, unlike the hour of the day.
	roam, err := ParseRequires("os=linux net.same_lan")
	if err != nil {
		t.Fatal(err)
	}
	if kp := KeyProbes(Skill{Requires: &roam}); !slices.Contains(kp, "net.same_lan") {
		t.Errorf("KeyProbes = %v; network locality must stay in the key", kp)
	}
}

func TestSharedRingPrecedence(t *testing.T) {
	shared := t.TempDir()
	writeSkill(t, shared, "team-skill", "---\nname: team-skill\ndescription: from the shared catalog\n---")
	writeSkill(t, shared, "alpha-notes", "---\nname: alpha-notes\ndescription: shared override\n---")
	local := t.TempDir()
	writeSkill(t, local, "alpha-notes", "---\nname: alpha-notes\ndescription: local override\n---")

	embedded := fstest.MapFS{
		"alpha-notes/SKILL.md": {Data: []byte("---\nname: alpha-notes\ndescription: embedded\n---\nbody\n")},
	}
	cat := &Catalog{Sources: []Source{
		EmbedSource(embedded, RingEmbedded),
		SharedDirSource(shared),
		DirSource(local),
	}}
	ps := testProbes(t, map[string]string{"os": "linux", "arch": "arm64"}, nil)
	rows, err := cat.List(ps)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Listing{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if r := byName["team-skill"]; r.Ring != RingShared || r.Ring.String() != "shared" {
		t.Errorf("team-skill: %+v", r)
	}
	// local > shared > embedded on collisions.
	if r := byName["alpha-notes"]; r.Ring != RingLocal || r.Description != "local override" || !r.Shadows {
		t.Errorf("alpha-notes: %+v", r)
	}
	// A missing shared dir is an empty source, not an error.
	cat2 := &Catalog{Sources: []Source{SharedDirSource(filepath.Join(shared, "missing"))}}
	if rows, err := cat2.List(ps); err != nil || len(rows) != 0 {
		t.Errorf("missing shared dir: %v %v", rows, err)
	}
}

func TestApplicableAdvertisement(t *testing.T) {
	shared := t.TempDir()
	long := strings.Repeat("very long description ", 20)
	writeSkill(t, shared, "team-skill", "---\nname: team-skill\ndescription: "+long+"\n---")
	writeSkill(t, shared, "elsewhere", "---\nname: elsewhere\ndescription: x\nmetadata:\n  requires: \"os=plan9\"\n---")
	ads := Applicable(WithSource(SharedDirSource(shared)), WithConfigDir(t.TempDir()))
	if len(ads) != 1 || ads[0].Name != "team-skill" || ads[0].Ring != "shared" || ads[0].Verified {
		t.Fatalf("ads = %+v", ads)
	}
	if len([]rune(ads[0].Description)) > 160 {
		t.Fatalf("description not truncated: %d runes", len([]rune(ads[0].Description)))
	}
}
