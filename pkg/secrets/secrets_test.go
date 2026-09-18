package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVault is an in-memory stand-in for cloudbox's /api/v1/secrets.
type fakeVault struct {
	t      *testing.T
	data   map[string]string
	server *httptest.Server
}

func newFakeVault(t *testing.T) *fakeVault {
	fv := &fakeVault{t: t, data: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad auth: "+got, http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			var items []Item
			for k, v := range fv.data {
				items = append(items, Item{Name: k, Value: v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"secrets": items})
		case http.MethodPost:
			var body struct {
				Secrets []Item `json:"secrets"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, s := range body.Secrets {
				fv.data[s.Name] = s.Value
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "count": len(body.Secrets)})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/secrets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/secrets/")
		switch r.Method {
		case http.MethodGet:
			v, ok := fv.data[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(Item{Name: name, Value: v})
		case http.MethodDelete:
			if _, ok := fv.data[name]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			delete(fv.data, name)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	fv.server = httptest.NewServer(mux)
	t.Cleanup(fv.server.Close)
	return fv
}

func (fv *fakeVault) cfg() Config { return Config{URL: fv.server.URL, Token: "test-token"} }

// run executes the secrets command tree with isolated stdout/stderr and a
// scratch HOME/XDG_CACHE so the cache lands in a tempdir.
func run(t *testing.T, cfg Config, args ...string) (string, string, error) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Neutralize ambient token/url discovery so tests are hermetic.
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "")

	cmd := newSecretsCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	full := append([]string{"--url", cfg.URL, "--token", cfg.Token}, args...)
	cmd.SetArgs(full)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func configureSecretsTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "")
}

func executeSecrets(t *testing.T, cfg Config, in io.Reader, args ...string) (string, string, error) {
	t.Helper()
	cmd := newSecretsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if in != nil {
		cmd.SetIn(in)
	}
	full := append([]string{"--url", cfg.URL, "--token", cfg.Token}, args...)
	cmd.SetArgs(full)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func writeDefaultTemplate(t *testing.T, contents string) {
	t.Helper()
	path := defaultTemplatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func digest(s string) [sha256.Size]byte {
	return sha256.Sum256([]byte(s))
}

func TestImportThenEnvRoundTrip(t *testing.T) {
	fv := newFakeVault(t)
	rc := `# a comment
export OPENAI_API_KEY=sk-proj-abc
export GITHUB_TOKEN=ghp_xyz
#export ANTHROPIC_API_KEY=should-be-skipped
KIMI_API_KEY="sk-quoted"
`
	rcFile := filepath.Join(t.TempDir(), ".novigensrc")
	if err := os.WriteFile(rcFile, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := run(t, fv.cfg(), "import", rcFile)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "imported 3 secret(s)") {
		t.Fatalf("import out = %q", out)
	}
	if _, ok := fv.data["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("commented line must not import")
	}
	if fv.data["KIMI_API_KEY"] != "sk-quoted" {
		t.Fatalf("quoted value = %q, want sk-quoted", fv.data["KIMI_API_KEY"])
	}

	// A binding template maps LOCAL env names to vault REFERENCES (the
	// tool owns naming/casing; GH_TOKEN renamed to prove the indirection).
	tmpl := filepath.Join(t.TempDir(), "secrets.map")
	if err := os.WriteFile(tmpl, []byte("# binding\nOPENAI_API_KEY=@OPENAI_API_KEY\nGH_TOKEN=@GITHUB_TOKEN\nKIMI=@{KIMI_API_KEY}\nEDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, fv.cfg(), "env", tmpl)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	// @refs resolve from the vault; bare EDITOR=vim passes through literal;
	// @{KIMI_API_KEY} brace form works too. Sorted by local name.
	want := "export EDITOR='vim'\nexport GH_TOKEN='ghp_xyz'\nexport KIMI='sk-quoted'\nexport OPENAI_API_KEY='sk-proj-abc'\n"
	if out != want {
		t.Fatalf("env out =\n%q\nwant\n%q", out, want)
	}
}

// TestEnvMissingRefSkipped: a template ref not present in the vault is
// skipped (with a stderr note), not fatal.
func TestEnvMissingRefSkipped(t *testing.T) {
	fv := newFakeVault(t)
	fv.data["OPENAI_API_KEY"] = "sk-ok"
	tmpl := filepath.Join(t.TempDir(), "secrets.map")
	if err := os.WriteFile(tmpl, []byte("OPENAI_API_KEY=@OPENAI_API_KEY\nGHOST=@does-not-exist\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := run(t, fv.cfg(), "env", tmpl)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if out != "export OPENAI_API_KEY='sk-ok'\n" {
		t.Fatalf("env out = %q", out)
	}
	if !strings.Contains(errOut, "GHOST") || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing-ref should warn on stderr, got %q", errOut)
	}
}

func TestEnvCacheFallbackWhenServerDown(t *testing.T) {
	fv := newFakeVault(t)
	fv.data["DEEPSEEK_API_KEY"] = "sk-d"

	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	// Default template lives under XDG_CONFIG_HOME/bashy/secrets.map.
	cfgdir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgdir)
	if err := os.MkdirAll(filepath.Join(cfgdir, "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgdir, "bashy", "secrets.map"), []byte("DEEPSEEK_API_KEY=@DEEPSEEK_API_KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "")

	// First env populates the cache.
	cmd := newSecretsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--url", fv.server.URL, "--token", "test-token", "env"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env(1): %v", err)
	}
	if !strings.Contains(out.String(), "export DEEPSEEK_API_KEY='sk-d'") {
		t.Fatal("env(1) render digest did not contain the vault fixture")
	}

	// Server goes away; the normal fetch-first path must fall back to cache
	// and still exit 0.
	fv.server.Close()
	cmd = newSecretsCmd()
	var out2, err2 bytes.Buffer
	cmd.SetOut(&out2)
	cmd.SetErr(&err2)
	cmd.SetArgs([]string{"--url", fv.server.URL, "--token", "test-token", "env"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env(2) should not error (degrade gracefully): %v", err)
	}
	if !strings.Contains(out2.String(), "export DEEPSEEK_API_KEY='sk-d'") {
		t.Fatal("env(2) cache fallback digest did not contain the cached fixture")
	}
	if !strings.Contains(out2.String(), "served from cache") {
		t.Fatal("env(2) did not identify the degraded cache fallback")
	}
}

func TestEnvRevalidatesReachableVault(t *testing.T) {
	fv := newFakeVault(t)
	configureSecretsTestEnv(t)
	writeDefaultTemplate(t, "OPENAI_API_KEY=@openai\n")

	const before = "synthetic-before"
	const after = "synthetic-after"
	fv.data["openai"] = before

	first, _, err := executeSecrets(t, fv.cfg(), nil, "env")
	if err != nil {
		t.Fatalf("first env: %v", err)
	}
	if digest(first) != digest("export OPENAI_API_KEY='"+before+"'\n") {
		t.Fatal("first env render digest did not match the vault fixture")
	}

	// Simulate a rotation performed by another process. The existing render
	// cache remains present, so only a reachable-vault revalidation can see it.
	fv.data["openai"] = after
	second, _, err := executeSecrets(t, fv.cfg(), nil, "env")
	if err != nil {
		t.Fatalf("second env: %v", err)
	}
	if digest(second) != digest("export OPENAI_API_KEY='"+after+"'\n") {
		t.Fatal("second env render digest was stale while cloudbox was reachable")
	}
}

func TestSetInvalidatesCacheAndRotationTakesEffect(t *testing.T) {
	fv := newFakeVault(t)
	configureSecretsTestEnv(t)
	writeDefaultTemplate(t, "OPENAI_API_KEY=@openai\n")

	const before = "synthetic-before"
	const after = "synthetic-after"
	fv.data["openai"] = before

	if _, _, err := executeSecrets(t, fv.cfg(), nil, "env"); err != nil {
		t.Fatalf("initial env: %v", err)
	}
	if _, err := os.Stat(cacheFile()); err != nil {
		t.Fatalf("initial env did not populate the render cache: %v", err)
	}

	if _, _, err := executeSecrets(t, fv.cfg(), nil, "set", "openai", after); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := os.Stat(cacheFile()); !os.IsNotExist(err) {
		t.Fatalf("set left the render cache present: %v", err)
	}

	got, _, err := executeSecrets(t, fv.cfg(), nil, "env")
	if err != nil {
		t.Fatalf("env after set: %v", err)
	}
	if digest(got) != digest("export OPENAI_API_KEY='"+after+"'\n") {
		t.Fatal("env render digest did not reflect the rotated value")
	}
}

func TestImportInvalidatesCache(t *testing.T) {
	fv := newFakeVault(t)
	configureSecretsTestEnv(t)
	if err := writeCache(cacheFile(), []byte("# synthetic render\n")); err != nil {
		t.Fatal(err)
	}

	const fixture = "synthetic-import"
	if _, _, err := executeSecrets(t, fv.cfg(), strings.NewReader("OPENAI_API_KEY="+fixture+"\n"), "import"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(cacheFile()); !os.IsNotExist(err) {
		t.Fatalf("import left the render cache present: %v", err)
	}
}

func TestRmInvalidatesCache(t *testing.T) {
	fv := newFakeVault(t)
	configureSecretsTestEnv(t)
	fv.data["openai"] = "synthetic-delete"
	if err := writeCache(cacheFile(), []byte("# synthetic render\n")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := executeSecrets(t, fv.cfg(), nil, "rm", "openai"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := os.Stat(cacheFile()); !os.IsNotExist(err) {
		t.Fatalf("rm left the render cache present: %v", err)
	}
}

func TestSecretsHelpPointsToAskWithoutAddingAlias(t *testing.T) {
	cmd := newSecretsCmd()
	var rootHelp bytes.Buffer
	cmd.SetOut(&rootHelp)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("secrets help: %v", err)
	}
	if !strings.Contains(rootHelp.String(), "bashy ask --name OPENAI_API_KEY --stdout | bashy secret set openai") {
		t.Fatal("secrets help does not show how to compose bashy ask with secrets set")
	}

	cmd = newSecretsCmd()
	var setHelp bytes.Buffer
	cmd.SetOut(&setHelp)
	cmd.SetArgs([]string{"set", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("secrets set help: %v", err)
	}
	if !strings.Contains(setHelp.String(), "bashy ask --name OPENAI_API_KEY --stdout | bashy secret set openai") {
		t.Fatal("secrets set help does not show how to compose bashy ask with secrets set")
	}
	for _, sub := range cmd.Commands() {
		if sub.Name() == "ask" {
			t.Fatal("secrets ask alias must not exist")
		}
	}
}

func TestGetSetRm(t *testing.T) {
	fv := newFakeVault(t)

	if _, _, err := run(t, fv.cfg(), "set", "TELEGRAM_BOT_TOKEN", "123:abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, _, err := run(t, fv.cfg(), "get", "TELEGRAM_BOT_TOKEN")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.TrimSpace(out) != "123:abc" {
		t.Fatalf("get = %q", out)
	}

	out, _, err = run(t, fv.cfg(), "ls")
	if err != nil || !strings.Contains(out, "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("ls = %q err=%v", out, err)
	}

	if _, _, err := run(t, fv.cfg(), "rm", "TELEGRAM_BOT_TOKEN"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, ok := fv.data["TELEGRAM_BOT_TOKEN"]; ok {
		t.Fatal("rm did not delete")
	}
}

func TestSetFromStdin(t *testing.T) {
	fv := newFakeVault(t)
	cmd := newSecretsCmd()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("sk-from-stdin\n"))
	cmd.SetArgs([]string{"--url", fv.server.URL, "--token", "test-token", "set", "OPENAI_API_KEY"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set stdin: %v", err)
	}
	if fv.data["OPENAI_API_KEY"] != "sk-from-stdin" {
		t.Fatalf("stdin value = %q (trailing newline must be trimmed)", fv.data["OPENAI_API_KEY"])
	}
}

func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"$(rm -rf)": "'$(rm -rf)'",
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEnvFileEdgeCases(t *testing.T) {
	in := `
export A=1
B=2
  export C = 3
export D=val # trailing comment
export E='quoted # hash'
export 9BAD=nope
# export F=skip
export G=
`
	items, err := parseEnvFile(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.Name] = it.Value
	}
	checks := map[string]string{
		"A": "1",
		"B": "2",
		"D": "val",
		"E": "quoted # hash",
		"G": "",
	}
	for k, v := range checks {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["F"]; ok {
		t.Error("commented F must be skipped")
	}
	if _, ok := got["9BAD"]; ok {
		t.Error("invalid name 9BAD must be skipped")
	}
	// "C = 3" -> name "C", value "3" (spaces trimmed both sides).
	if got["C"] != "3" {
		t.Errorf("C = %q, want 3", got["C"])
	}
}

func TestResolveTokenPrecedence(t *testing.T) {
	t.Setenv("BASHY_SECRETS_TOKEN", "from-bashy")
	t.Setenv("BASHY_API_KEY", "from-apikey")
	c, err := Config{URL: "http://x"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "from-bashy" {
		t.Fatalf("token = %q, want from-bashy (BASHY_SECRETS_TOKEN wins)", c.Token)
	}
	// Flag beats env.
	c, _ = Config{URL: "http://x", Token: "from-flag"}.Resolve()
	if c.Token != "from-flag" {
		t.Fatalf("token = %q, want from-flag", c.Token)
	}
}

func TestResolveTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	if err := os.MkdirAll(filepath.Join(dir, "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bashy", "secrets-token"), []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Config{URL: "http://x"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "file-token" {
		t.Fatalf("token = %q, want file-token (trimmed) from ~/.config/bashy/secrets-token", c.Token)
	}
	// $BASHY_SECRETS_TOKEN must still win over the file.
	t.Setenv("BASHY_SECRETS_TOKEN", "env-token")
	c, _ = Config{URL: "http://x"}.Resolve()
	if c.Token != "env-token" {
		t.Fatalf("token = %q, want env-token (env beats file)", c.Token)
	}
}

func TestResolveURLFromEnv(t *testing.T) {
	t.Setenv("BASHY_CLOUDBOX_URL", "https://box.example/")
	c, err := Config{Token: "x"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://box.example" {
		t.Fatalf("baseURL = %q, want https://box.example ($BASHY_CLOUDBOX_URL, trailing slash trimmed)", c.BaseURL)
	}
	// Default when unset.
	t.Setenv("BASHY_CLOUDBOX_URL", "")
	c, _ = Config{Token: "x"}.Resolve()
	if c.BaseURL != "https://ai.dhnt.io" {
		t.Fatalf("baseURL = %q, want the default https://ai.dhnt.io", c.BaseURL)
	}
}

// guard: the JSON encoder used by the fake is the real wire shape.
var _ = io.Discard
