package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// refCatalog writes a small local store and returns a registry resolving
// over it. Hermetic: the root is a tempdir and WithRoot pins every noun
// (and the skills ring) inside it, ignoring ambient overrides.
func refCatalog(t *testing.T) *ref.Registry {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_HOME", root)
	t.Setenv("BASHY_SKILLS_DIR", "")
	// WithRoot pins the local store but not the read-only shared rings; an
	// ambient $BASHY_*_PATH (a weave harness exports the umbrella fleet)
	// would merge foreign entries into the catalog under test.
	for _, env := range []string{"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_PEOPLE_PATH", "BASHY_HOSTS_PATH"} {
		t.Setenv(env, "")
	}

	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("models/opus4.yaml", "name: opus4\ndisplay: Opus 4\nfamily: opus\nversion: \"4\"\n")
	write("models/opus5.yaml", "name: opus5\ndisplay: Opus 5\nfamily: opus\nversion: \"5\"\n")
	write("tools/mytool.yaml", "name: mytool\nkind: cli\ndisplay: My Tool\n")
	write("agents/buddy.yaml", "agents:\n  - name: buddy\n    display: Buddy\n    tool: mytool\n    model: opus5\n")
	write("hosts/box1.yaml", "name: box1\ndisplay: Box One\naddress: box1.internal\n")
	write("skills/deploy/SKILL.md", "---\nname: deploy\ndescription: Deploy the app\n---\n# Deploy\n")

	g := ref.NewRegistry()
	RegisterRefs(g, WithRoot(root), WithoutCloudOverlay())
	return g
}

func TestRegisterRefsResolvesEveryFleetKind(t *testing.T) {
	g := refCatalog(t)
	for _, tt := range []struct {
		query, wantRef, wantTitle, wantOpen string
	}{
		{"agent:buddy", "agent:buddy", "Buddy", "bashy agent show buddy"},
		{"tool:mytool", "tool:mytool", "My Tool", "bashy tool show mytool"},
		{"model:opus5", "model:opus5", "Opus 5", "bashy model show opus5"},
		{"host:box1", "host:box1", "Box One", "bashy whois box1"},
		{"skill:deploy", "skill:deploy", "Deploy the app", "bashy skill show deploy"},
	} {
		n, err := g.Resolve(tt.query)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tt.query, err)
		}
		if n.Ref != tt.wantRef || n.Title != tt.wantTitle || n.Open != tt.wantOpen {
			t.Errorf("Resolve(%q) = ref %q title %q open %q, want %q / %q / %q",
				tt.query, n.Ref, n.Title, n.Open, tt.wantRef, tt.wantTitle, tt.wantOpen)
		}
		if n.Status == "" || n.Where == "" {
			t.Errorf("Resolve(%q) left Status/Where empty: %+v", tt.query, n)
		}
	}

	// The skill node points at the record itself, not at a ring label.
	n, err := g.Resolve("skill:deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(n.Where, filepath.Join("deploy", "SKILL.md")) {
		t.Errorf("skill Where = %q, want the SKILL.md path", n.Where)
	}
	if n.Status != "local" {
		t.Errorf("skill Status = %q, want local", n.Status)
	}
}

// The edge the story names: a bare family alias resolves to the derived
// highest-version record — and the node says so, because "opus" is a
// floating pointer, not a record of its own.
func TestRefModelFamilyAliasResolvesToNewest(t *testing.T) {
	g := refCatalog(t)
	n, err := g.Resolve("model:opus")
	if err != nil {
		t.Fatalf("family alias did not resolve: %v", err)
	}
	if n.ID != "opus5" || n.Ref != "model:opus5" {
		t.Fatalf("model:opus = %q, want the newest member model:opus5", n.Ref)
	}
	if !strings.Contains(n.Title, "alias opus") {
		t.Errorf("Title %q does not say the alias was resolved", n.Title)
	}
}

func TestRefUnknownNamesAreNotFound(t *testing.T) {
	g := refCatalog(t)
	for _, q := range []string{"agent:nosuch", "tool:nosuch", "model:nosuch", "host:nosuch", "skill:nosuch", "skill:../escape"} {
		_, err := g.Resolve(q)
		if !errors.Is(err, ref.ErrNotFound) {
			t.Errorf("Resolve(%q) err = %v, want ErrNotFound", q, err)
		}
	}
}
