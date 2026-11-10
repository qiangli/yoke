package principal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestOwnerFromPairing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(p, []byte(`{"agent_name":"studio","owner_email":"Alice@Example.com"}`), 0600); err != nil {
		t.Fatal(err)
	}
	email := readOutpostOwnerEmail(p)
	if email != "alice@example.com" {
		t.Fatal(email)
	}
	cat := fleet.New()
	paired := NewResolver(cat, Env{Paired: true, PairedEmail: email})
	if paired.owner != email {
		t.Fatal(paired.owner)
	}
	local := NewResolver(cat, Env{})
	if local.owner != LocalOwner {
		t.Fatal(local.owner)
	}
}
