package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// principalFleet writes one agent and one person into a scratch catalog.
func principalFleet(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	mk := func(sub, file, body string) {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, sub, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("agents", "scout.yaml", "name: scout\ntool: claude\nmodel: opus5\n")
	mk("people", "operator.yaml", "handle: operator\ndisplay: The Operator\naliases: [op]\n")
	return New(WithRoot(root))
}

// A person must be able to OWN work. This is the defect the human lane exists
// to fix: the board accepted a human name while todo and sprint refused it.
func TestResolvePrincipalAcceptsAPerson(t *testing.T) {
	cat := principalFleet(t)
	for _, in := range []string{"operator", "OPERATOR", "op"} {
		got, kind, err := cat.ResolvePrincipal(in)
		if err != nil {
			t.Fatalf("ResolvePrincipal(%q) = %v, want a person", in, err)
		}
		if got != "operator" || kind != KindPerson {
			t.Errorf("ResolvePrincipal(%q) = %q/%s, want operator/person", in, got, kind)
		}
	}
}

func TestResolvePrincipalStillResolvesAnAgent(t *testing.T) {
	cat := principalFleet(t)
	got, kind, err := cat.ResolvePrincipal("scout")
	if err != nil || got != "scout" || kind != KindAgent {
		t.Fatalf("ResolvePrincipal(scout) = %q/%s/%v, want scout/agent/nil", got, kind, err)
	}
}

// Unknown and ambiguous are different answers and must stay different: one says
// register it, the other says qualify it.
func TestResolvePrincipalSeparatesUnknownFromAmbiguous(t *testing.T) {
	cat := principalFleet(t)
	if _, _, err := cat.ResolvePrincipal("nobody"); !errors.Is(err, ErrPrincipalUnknown) {
		t.Errorf("unknown name: err = %v, want ErrPrincipalUnknown", err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two ENTRIES sharing one canonical name — the collision that a name-keyed
	// count would have folded back into "unknown".
	for _, f := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(root, "agents", f),
			[]byte("name: twin\ntool: claude\nmodel: opus5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := New(WithRoot(root)).ResolvePrincipal("twin"); !errors.Is(err, ErrPrincipalAmbiguous) {
		t.Errorf("duplicated name: err = %v, want ErrPrincipalAmbiguous", err)
	}
}
