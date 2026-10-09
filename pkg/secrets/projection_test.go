package secrets

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestProjectAgentEnvBindingSources(t *testing.T) {
	for _, mode := range []string{"literal", "vault", "offline-cache", "deleted-vault-key", "parent-wins", "no-contract", "alias"} {
		t.Run(mode, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfg)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv("BASHY_SECRETS_TOKEN", "fixture-token")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/v1/secrets" {
					t.Errorf("unexpected vault path %q", r.URL.Path)
				}
				if mode == "offline-cache" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "deleted-vault-key" {
					_, _ = w.Write([]byte(`{"secrets":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"secrets":[{"name":"provider-glm","value":"vault-fixture"},{"name":"private","value":"unrelated"}]}`))
			}))
			defer server.Close()
			t.Setenv("BASHY_CLOUDBOX_URL", server.URL)
			if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0700); err != nil {
				t.Fatal(err)
			}
			value := "@provider-glm"
			want := "vault-fixture"
			if mode == "literal" {
				value = "'literal-fixture'"
				want = "literal-fixture"
			}
			if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte("ZAI_API_KEY="+value+"\nPRIVATE_TOKEN=@private\nORDINARY_NAME=private\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := writeCache(cacheFile(), []byte("export ZAI_API_KEY='cached-fixture'\nexport PRIVATE_TOKEN='private'\nexport OLD_KEY='obsolete'\n")); err != nil {
				t.Fatal(err)
			}
			if mode == "offline-cache" {
				want = "cached-fixture"
			}
			parent := []string{"PATH=/bin"}
			names := []string{"ZAI_API_KEY"}
			var aliases map[string][]string
			target := "ZAI_API_KEY"
			if mode == "parent-wins" {
				parent = append(parent, "ZAI_API_KEY=parent-fixture")
				want = "parent-fixture"
			}
			if mode == "no-contract" {
				names = nil
			}
			if mode == "alias" {
				names = nil
				aliases = map[string][]string{"CLI_API_KEY": {"ZAI_API_KEY"}}
				target = "CLI_API_KEY"
			}
			got := ProjectAgentEnv([]string{"PATH=/bin"}, parent, names, aliases)
			if mode == "deleted-vault-key" || mode == "no-contract" {
				if len(got) != 1 {
					t.Fatal("credential granted without authoritative binding")
				}
			} else if !slices.Contains(got, target+"="+want) || len(got) != 2 {
				t.Fatal("selected credential projection differs from contract")
			}
			if (mode == "literal" || mode == "parent-wins" || mode == "no-contract") && calls != 0 {
				t.Fatal("unnecessary vault access")
			}
		})
	}
}
