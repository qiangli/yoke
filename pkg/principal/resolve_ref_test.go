package principal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ref"
)

// refTestResolver builds a resolver over a catalog holding one person.
// Hermetic: the fleet root is a tempdir and the env is the stubbed testEnv.
func refTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_HOME", root)
	// WithRoot pins the local store but not the read-only shared rings; an
	// ambient $BASHY_*_PATH (a weave harness exports the umbrella fleet)
	// would merge foreign entries into the catalog under test.
	for _, env := range []string{"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_PEOPLE_PATH", "BASHY_HOSTS_PATH"} {
		t.Setenv(env, "")
	}
	if err := os.MkdirAll(filepath.Join(root, "people"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("handle: alice\ndisplay: Alice A\n")
	if err := os.WriteFile(filepath.Join(root, "people", "alice.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := testResolver(t, testEnv(t), fleet.WithRoot(root))
	return r, root
}

func TestRefPersonResolves(t *testing.T) {
	r, _ := refTestResolver(t)
	n, err := r.refPerson("alice")
	if err != nil {
		t.Fatalf("refPerson: %v", err)
	}
	if n.Ref != "person:alice" || n.Title != "Alice A" || n.Open != "bashy whois alice" {
		t.Errorf("node = ref %q title %q open %q, want person:alice / Alice A / bashy whois alice",
			n.Ref, n.Title, n.Open)
	}
	if n.Where != SourceFleet {
		t.Errorf("Where = %q, want the identity source %q", n.Where, SourceFleet)
	}

	if _, err := r.refPerson("bob"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("refPerson(bob) err = %v, want ErrNotFound", err)
	}
}

// The edge the story names: role:steward resolves to the SEAT and reports
// its holder or its vacancy — the same two spellings the addresser accepts.
func TestRefRoleReportsHolderOrVacant(t *testing.T) {
	r, _ := refTestResolver(t)
	withRoles(t,
		HostRole{Label: "steward", Topic: "steward.host-1", Holder: "al"},
		HostRole{Label: "conductor:22", Topic: "conductor-22.host-1"},
	)

	for _, q := range []string{"steward", "steward.host-1"} {
		n, err := r.refRole(q)
		if err != nil {
			t.Fatalf("refRole(%q): %v", q, err)
		}
		if n.Ref != "role:steward" || n.Status != "held" || n.Title != "held by al" {
			t.Errorf("refRole(%q) = ref %q status %q title %q, want the held steward seat", q, n.Ref, n.Status, n.Title)
		}
		if n.Where != "steward.host-1" {
			t.Errorf("refRole(%q) Where = %q, want the seat topic", q, n.Where)
		}
	}

	n, err := r.refRole("conductor:22")
	if err != nil {
		t.Fatalf("refRole(conductor:22): %v", err)
	}
	if n.Status != "vacant" || n.Title != "vacant seat" {
		t.Errorf("vacant seat = status %q title %q, want vacant / vacant seat", n.Status, n.Title)
	}

	if _, err := r.refRole("nosuch"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("refRole(nosuch) err = %v, want ErrNotFound", err)
	}
}

func TestRegisterRefsWiresPersonAndRole(t *testing.T) {
	_, root := refTestResolver(t)
	withRoles(t, HostRole{Label: "steward", Topic: "steward.host-1"})

	g := ref.NewRegistry()
	RegisterRefs(g, fleet.WithRoot(root))
	n, err := g.Resolve("person:alice")
	if err != nil || n.Ref != "person:alice" {
		t.Fatalf("Resolve(person:alice) = %+v, %v", n, err)
	}
	if n, err := g.Resolve("role:steward"); err != nil || n.Status != "vacant" {
		t.Fatalf("Resolve(role:steward) = %+v, %v", n, err)
	}
}

// whois answers — text and JSON — carry the uniform ref, so a reader can
// quote the record it was told about without re-deriving the spelling.
func TestWhoisAnswerCarriesTheRef(t *testing.T) {
	r, _ := refTestResolver(t)
	ans := r.Resolve("alice")
	if !ans.Resolved {
		t.Fatal("alice did not resolve")
	}
	res := ans.Matches[0]
	if res.Canonical != "person:alice" {
		t.Fatalf("Canonical = %q, want person:alice", res.Canonical)
	}

	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"ref":"person:alice"`) {
		t.Errorf("JSON %s does not carry the ref field", blob)
	}

	var sb strings.Builder
	printResolution(&sb, res)
	if !strings.Contains(sb.String(), "ref:") || !strings.Contains(sb.String(), "person:alice") {
		t.Errorf("text output missing the ref line:\n%s", sb.String())
	}
}
