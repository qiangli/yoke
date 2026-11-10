package cloudbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

// S606 S2: the canonical outpost config — one cloudbox pairing plus the
// satellites attached under it — is enumerated as the primary account and
// one satellite account per attachment.
const canonicalAgent = `{
  "agent_name": "studio",
  "cloudbox": {"portal_url": "https://portal.example/", "host": "studio",
    "token": "` + accountstest.CanarySecret + `", "scopes": ["host"],
    "owner_email": "alice@example.com"},
  "attachments": [
    {"satellite_id": "sat_1", "name": "zoo", "endpoint": "https://zoo.example",
     "token": "` + accountstest.CanarySecret + `", "scopes": ["satellite-host"]},
    {"satellite_id": "sat_2", "endpoint": "https://lab.example:8443/",
     "token": "` + accountstest.CanarySecret + `"}
  ]
}`

func TestDiscoverCanonicalPairingAndAttachments(t *testing.T) {
	outpost := t.TempDir()
	write(t, filepath.Join(outpost, "agent.json"), canonicalAgent)
	down := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("connection refused") }
	a := &Adapter{ConfigDir: t.TempDir(), OutpostDir: outpost, Exec: down}

	obs, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(obs)
	if strings.Contains(string(raw), accountstest.CanarySecret) {
		t.Fatal("a token reached an observation")
	}
	if len(obs) != 3 {
		t.Fatalf("observations = %d, want primary + 2 attachments: %s", len(obs), raw)
	}

	p := find(obs, "https://portal.example|alice@example.com")
	if p.Category != accounts.Cloudbox || p.Name != "studio" || p.Username != "alice@example.com" || p.Status != accounts.Resolved || p.LastError != "" {
		t.Fatalf("primary = %+v", p)
	}
	if len(p.Connections) != 1 || p.Connections[0].Endpoint != "https://portal.example" || p.Connections[0].CredentialRef != "" {
		t.Fatalf("primary connections = %+v", p.Connections)
	}
	if got := strings.Join(p.Links.Satellites, ","); got != "sat_2,zoo" {
		t.Fatalf("primary satellites = %q", got)
	}
	if got := strings.Join(p.Links.Hosts, ","); got != "studio" {
		t.Fatalf("primary hosts = %q", got)
	}

	zoo := find(obs, "https://zoo.example|alice@example.com")
	if zoo.Category != accounts.Satellite || zoo.Name != "zoo" || zoo.Connections[0].ID != "sat_1" || zoo.Status != accounts.Resolved {
		t.Fatalf("zoo = %+v", zoo)
	}
	lab := find(obs, "https://lab.example:8443|alice@example.com")
	if lab.Category != accounts.Satellite || lab.Name != "sat_2" || lab.Connections[0].Endpoint != "https://lab.example:8443" {
		t.Fatalf("lab = %+v", lab)
	}
}

// A legacy profile on the same portal and user merges into the primary
// account instead of duplicating it.
func TestDiscoverCanonicalMergesLegacyProfile(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".active"), "p1\n")
	write(t, filepath.Join(root, "agent.json"), `{"agent_name":"p1","user":"alice@example.com","portal_url":"https://portal.example"}`)
	outpost := t.TempDir()
	write(t, filepath.Join(outpost, "agent.json"), canonicalAgent)
	a := &Adapter{ConfigDir: root, OutpostDir: outpost, Exec: okExec}

	obs, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := find(obs, "https://portal.example|alice@example.com")
	if len(p.Connections) != 2 || p.Category != accounts.Cloudbox {
		t.Fatalf("primary = %+v", p)
	}
	if len(obs) != 3 {
		t.Fatalf("observations = %d", len(obs))
	}
}

// Without a cloudbox block nothing new appears (legacy hosts are unchanged).
func TestDiscoverNoCanonicalBlockIsUnchanged(t *testing.T) {
	outpost := t.TempDir()
	write(t, filepath.Join(outpost, "agent.json"), `{"agent_name":"x","server_addr":"sat.example"}`)
	a := &Adapter{ConfigDir: t.TempDir(), OutpostDir: outpost, CachePath: filepath.Join(t.TempDir(), "c.json"),
		Exec: func(context.Context, ...string) ([]byte, error) { return nil, errors.New("not installed") }}
	obs, err := a.Discover(context.Background())
	if err != nil || len(obs) != 0 {
		t.Fatalf("obs=%v err=%v", obs, err)
	}
}
