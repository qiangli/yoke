package weave

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/secrets"
)

func TestWeaveChildEnvProjectsOnlyBindingSecretsMap(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte("ZAI_API_KEY='fixture-glm'\nUNRELATED_SECRET='fixture-private'\nORDINARY_NAME='also-private'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = old })
	l, err := weaveResolveAgent("ycode:glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	env := weaveChildEnv([]string{"PATH=/bin"}, "/workspace", "agent/test", "main", t.TempDir(), &weaveItem{ID: 13, Owner: "fixture"}, l)
	if !slices.Contains(env, "ZAI_API_KEY=fixture-glm") {
		t.Fatal("selected map-only credential missing from weave environment")
	}
	for _, kv := range env {
		if kv == "UNRELATED_SECRET=fixture-private" || kv == "ORDINARY_NAME=also-private" {
			t.Fatal("ungranted map entry escaped into child")
		}
	}
}
