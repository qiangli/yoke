package chat

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/secrets"
)

func TestForemanChildEnvProjectsOnlyBindingSecretsMap(t *testing.T) {
	pinCatalog(t)
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("CLOUDBOX_TOKEN", "fixture-pairing")
	t.Setenv("ZAI_API_KEY", "")
	if err := os.Unsetenv("ZAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte("ZAI_API_KEY='fixture-glm'\nUNRELATED_SECRET='fixture-private'\nORDINARY_NAME='also-private'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := resolveLaunch("ycode:glm-5.2", Options{AllowUnsafe: true})
	if err != nil {
		t.Fatal(err)
	}
	env := agentChildEnv(withLaunch(context.Background(), l))
	if !slices.Contains(env, "ZAI_API_KEY=fixture-glm") {
		t.Fatal("selected map-only credential missing from foreman/chat environment")
	}
	for _, kv := range env {
		if kv == "UNRELATED_SECRET=fixture-private" || kv == "ORDINARY_NAME=also-private" {
			t.Fatal("ungranted map entry escaped into child")
		}
	}
	if _, ok := os.LookupEnv("ZAI_API_KEY"); ok {
		t.Fatal("projection mutated parent environment")
	}
}
