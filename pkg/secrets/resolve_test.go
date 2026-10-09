package secrets

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Sprint: #379; Story: #37; Story-ID: 5b537ed16256
//
// The catalog names a credential by a STANDARD ref (`api_key_ref: zai`); a host
// binds that ref under its own vault name (`ZAI_API_KEY=@host-zai` in
// secrets.map). A caller that looked the ref up in the vault by its bare name
// (`bashy secret get zai`) found nothing on every host whose vault names are
// host-prefixed — which is every host set up by the template. The resolution
// has to go ref -> conventional env names -> the host's binding -> the vault.
func TestResolveAgentKeyThroughHostBinding(t *testing.T) {
	for _, mode := range []string{"vault", "offline-cache", "env-wins", "env-empty", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfg)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv("BASHY_SECRETS_TOKEN", "fixture-token")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "offline-cache" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// The vault knows the HOST name only; there is no secret called "zai".
				_, _ = w.Write([]byte(`{"secrets":[{"name":"host-zai","value":"vault-fixture"},{"name":"host-github","value":"unrelated"}]}`))
			}))
			defer server.Close()
			t.Setenv("BASHY_CLOUDBOX_URL", server.URL)
			if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0700); err != nil {
				t.Fatal(err)
			}
			tmpl := "ZAI_API_KEY=@host-zai\nGITHUB_TOKEN=@host-github\n"
			if mode == "unbound" {
				tmpl = "GITHUB_TOKEN=@host-github\n"
			}
			if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte(tmpl), 0600); err != nil {
				t.Fatal(err)
			}
			if err := writeCache(cacheFile(), []byte("export ZAI_API_KEY='cached-fixture'\n")); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=/bin"}
			want := "ZAI_API_KEY=vault-fixture"
			switch mode {
			case "offline-cache":
				want = "ZAI_API_KEY=cached-fixture"
			case "env-wins":
				env = append(env, "ZAI_API_KEY=env-fixture")
				want = "ZAI_API_KEY=env-fixture"
			case "env-empty":
				// Exported but empty: a profile that ran before the vault was up.
				env = append(env, "ZAI_API_KEY=", "ZAI_TOKEN= ")
			}
			got, ok := ResolveAgentKey(env, "zai")
			if mode == "unbound" {
				if ok {
					t.Fatalf("a ref this host does not bind must not resolve, got %q", got)
				}
				return
			}
			if !ok || got != want {
				t.Fatalf("ResolveAgentKey(zai) = %q, %v; want %q", got, ok, want)
			}
			if mode == "env-wins" && calls != 0 {
				t.Fatal("the vault was consulted although the environment already carried the key")
			}
			if mode != "env-wins" && mode != "offline-cache" && calls != 1 {
				t.Fatalf("vault calls = %d, want exactly 1", calls)
			}
		})
	}
}

// It grants ONE key: resolving `zai` through the binding must not drag the
// rest of the template along.
func TestResolveAgentKeyGrantsNothingElse(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "fixture-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":[{"name":"host-zai","value":"z"},{"name":"host-github","value":"g"}]}`))
	}))
	defer server.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", server.URL)
	if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte("ZAI_API_KEY=@host-zai\nGITHUB_TOKEN=@host-github\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolveAgentKey([]string{"PATH=/bin"}, "zai")
	if !ok || got != "ZAI_API_KEY=z" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if _, ok := ResolveAgentKey([]string{"PATH=/bin"}, ""); ok {
		t.Fatal("an empty ref must not resolve")
	}
}
