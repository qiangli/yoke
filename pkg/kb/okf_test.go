package kb

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// okfFixtureRing writes a small ring: three pages (one validated) and typed
// relations among them, plus one to a code entity and one forgotten.
func okfFixtureRing(t *testing.T, dir string) {
	t.Helper()
	s := Open(dir)
	for _, p := range []*Page{
		{Slug: "alpha", Type: TypeGotcha, Title: "Alpha gotcha", Description: "WHEN alpha", Tags: []string{"a"}, Status: StatusValidated, Body: "alpha body\n\nsee [[beta]]"},
		{Slug: "beta", Type: TypeDecision, Title: "Beta decision", Description: "WHEN beta", Body: "beta body"},
		{Slug: "gamma", Type: TypeFact, Title: "Gamma fact", Description: "WHEN gamma", Body: "gamma body"},
	} {
		if err := s.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	writeRelations(t, dir,
		`{"id":"r1","op":"link","target":"kb:alpha","relation":"about","dst":"kb:beta"}`,
		`{"id":"r2","op":"link","target":"kb:alpha","relation":"contradicts","dst":"kb:gamma"}`,
		`{"id":"r3","op":"link","target":"kb:beta","relation":"part-of","dst":"kb:gamma"}`,
		`{"id":"r4","op":"link","target":"kb:alpha","relation":"calls","dst":"code:pkg/x.Foo"}`,
		`{"id":"r5","op":"link","target":"kb:gamma","relation":"supports","dst":"kb:alpha"}`,
		`{"id":"r5","op":"forget","forget_id":"r5"}`,
	)
}

type relKey struct{ src, rel, dst string }

func liveRelKeys(t *testing.T, dir string) []relKey {
	t.Helper()
	live, err := (RelationRing{Dir: dir}).Live()
	if err != nil {
		t.Fatal(err)
	}
	var out []relKey
	for _, r := range live {
		if r.Op == "link" {
			out = append(out, relKey{r.Target, r.Relation, r.Dst})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.src+a.rel+a.dst < b.src+b.rel+b.dst
	})
	return out
}

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// checkOKFBundle applies the OKF reference samples' conventions: type on
// every file, index.md and log.md present, every relative link resolves.
func checkOKFBundle(t *testing.T, bundle string) []string {
	t.Helper()
	for _, f := range []string{"index.md", "log.md"} {
		if _, err := os.Stat(filepath.Join(bundle, f)); err != nil {
			t.Errorf("bundle missing %s: %v", f, err)
		}
	}
	var files []string
	err := filepath.WalkDir(bundle, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(bundle, p)
		files = append(files, filepath.ToSlash(rel))
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fm, body, ok := splitFrontmatter(b)
		if !ok || !regexp.MustCompile(`(?m)^type: \S`).MatchString(fm) {
			t.Errorf("%s: no type in frontmatter", rel)
		}
		for _, m := range mdLink.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.Contains(target, "://") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), filepath.FromSlash(target))); err != nil {
				t.Errorf("%s: link %s does not resolve", rel, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestOKFExportImportRoundTrip(t *testing.T) {
	isolateRelationStores(t)
	src := t.TempDir()
	okfFixtureRing(t, src)
	bundle := filepath.Join(t.TempDir(), "bundle")
	mustRun(t, src, "export", "--okf", bundle)

	files := checkOKFBundle(t, bundle)
	want := []string{"index.md", "log.md", "pages/alpha.md", "pages/beta.md", "pages/gamma.md"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("bundle files = %v, want %v", files, want)
	}

	alpha, err := os.ReadFile(filepath.Join(bundle, "pages", "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"resource: kb:beta", "timestamp: ", "## about\n\n- [Beta decision](beta.md)",
		"## calls\n\n- `code:pkg/x.Foo`", "## contradicts\n\n- [Gamma fact](gamma.md)",
	} {
		if !strings.Contains(string(alpha), s) {
			t.Errorf("alpha.md missing %q:\n%s", s, alpha)
		}
	}
	gamma, _ := os.ReadFile(filepath.Join(bundle, "pages", "gamma.md"))
	if strings.Contains(string(gamma), "## supports") || !strings.Contains(string(gamma), "resource: kb:gamma") {
		t.Errorf("gamma.md: forgotten relation exported or resource wrong:\n%s", gamma)
	}
	logmd, _ := os.ReadFile(filepath.Join(bundle, "log.md"))
	if !strings.Contains(string(logmd), "add [alpha](pages/alpha.md)") {
		t.Errorf("log.md not generated from the journal:\n%s", logmd)
	}

	dst := t.TempDir()
	out := mustRun(t, dst, "import", "--okf", bundle)
	if !strings.Contains(out, "imported 3 page(s) as candidate, 4 relation(s)") {
		t.Fatalf("import output: %s", out)
	}
	orig, err := Open(src).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orig {
		got, err := Open(dst).Load(o.Slug)
		if err != nil {
			t.Fatalf("imported %s: %v", o.Slug, err)
		}
		if got.Type != o.Type || got.Title != o.Title || got.Description != o.Description || got.Body != o.Body || got.ID != o.ID {
			t.Errorf("%s did not round-trip:\n got %+v\nwant %+v", o.Slug, got, o)
		}
		if got.Status != StatusCandidate {
			t.Errorf("%s imported as %s, want candidate", o.Slug, got.Status)
		}
		if !hasXferTag(got.Tags, "okf") || got.Source == nil || got.Source.Tool == "" {
			t.Errorf("%s: source not stamped: tags=%v source=%+v", o.Slug, got.Tags, got.Source)
		}
	}
	if a, b := liveRelKeys(t, src), liveRelKeys(t, dst); len(a) != len(b) || strings.Join(relStrings(a), ";") != strings.Join(relStrings(b), ";") {
		t.Errorf("relations did not round-trip:\n src %v\n dst %v", a, b)
	}

	// Re-import is idempotent: existing slugs skipped, relation ids replay to one.
	out = mustRun(t, dst, "import", "--okf", bundle)
	if !strings.Contains(out, "imported 0 page(s)") || !strings.Contains(out, "skipped alpha") {
		t.Errorf("re-import output: %s", out)
	}
}

func relStrings(ks []relKey) []string {
	var out []string
	for _, k := range ks {
		out = append(out, k.src+" "+k.rel+" "+k.dst)
	}
	return out
}

// TestOKFExportNeverExportsAgentRing: the agent ring refuses export, and a
// host-ring bundle carries none of its pages.
func TestOKFExportNeverExportsAgentRing(t *testing.T) {
	root := isolateRelationStores(t)
	t.Setenv("BASHY_AGENT", "w1")
	agent := OpenAgentRing(AgentRingDir(), "w1")
	if err := agent.Write(&Page{Slug: "private-note", Type: TypeLesson, Title: "private", Description: "mine", Source: &Source{Tool: "w1"}}, "add"); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(root, "host-kb")
	if err := Open(host).Write(&Page{Slug: "shared", Type: TypeFact, Title: "shared", Description: "ours"}, "add"); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "b")
	if _, err := run(t, "", "", "--ring", "agent", "export", "--okf", bundle); err == nil {
		t.Fatal("export --ring agent must fail")
	}
	if _, err := ExportOKF(agent, bundle); err == nil {
		t.Fatal("ExportOKF on the agent ring must fail")
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("refused export left a bundle behind: %v", err)
	}

	if _, err := ExportOKF(Open(host), bundle); err != nil {
		t.Fatal(err)
	}
	files := checkOKFBundle(t, bundle)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(bundle, f))
		if strings.Contains(string(b), "private-note") {
			t.Errorf("agent-ring page leaked into %s", f)
		}
	}
}
