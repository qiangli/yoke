package secrets

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// stubBindingCmd replaces the command runner for one test; each call returns
// the next value from mint, or err when set.
func stubBindingCmd(t *testing.T, err error, mint ...string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := runBindingCmd
	runBindingCmd = func(argv []string, _ io.Writer) (string, error) {
		calls = append(calls, argv)
		if err != nil {
			return "", err
		}
		return mint[(len(calls)-1)%len(mint)], nil
	}
	t.Cleanup(func() { runBindingCmd = prev })
	return &calls
}

func writeSecretsMap(t *testing.T, body string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cfg, "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bashy", "secrets.map"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadTemplateCommandBinding(t *testing.T) {
	writeSecretsMap(t, "A=!bashy bedrock-key --expires 3600  # minted\nB='!not-a-command'\nC=!\nD=@vault-ref\n")
	got, err := readTemplate(defaultTemplatePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("bindings = %+v, want A, B, D (an empty command is skipped)", got)
	}
	if !got[0].isCmd || !slices.Equal(got[0].cmd, []string{"bashy", "bedrock-key", "--expires", "3600"}) {
		t.Fatalf("A = %+v", got[0])
	}
	if got[1].isCmd || got[1].literal != "!not-a-command" {
		t.Fatalf("a quoted '!' stays a literal: %+v", got[1])
	}
	if !got[2].isRef || got[2].ref != "vault-ref" {
		t.Fatalf("D = %+v", got[2])
	}
}

// A launch projects a freshly minted value under the tool's own name (the
// credential_env alias), runs the command once per launch, and never needs
// the vault for it.
func TestProjectAgentEnvMintsPerLaunch(t *testing.T) {
	writeSecretsMap(t, "AWS_BEDROCK_API_KEY=!bashy bedrock-key\n")
	t.Setenv("BASHY_CLOUDBOX_URL", "http://127.0.0.1:1") // any vault call would fail
	calls := stubBindingCmd(t, nil, "minted-1", "minted-2")
	aliases := map[string][]string{"AWS_BEARER_TOKEN_BEDROCK": CredentialEnvNames("aws-bedrock")}
	for _, want := range []string{"minted-1", "minted-2"} {
		env := ProjectAgentEnv([]string{"PATH=/bin"}, []string{"PATH=/bin"}, nil, aliases)
		if !slices.Contains(env, "AWS_BEARER_TOKEN_BEDROCK="+want) {
			t.Fatalf("env = %v, want AWS_BEARER_TOKEN_BEDROCK=%s", env, want)
		}
	}
	if len(*calls) != 2 || !slices.Equal((*calls)[0], []string{"bashy", "bedrock-key"}) {
		t.Fatalf("calls = %v", *calls)
	}
	kv, ok := ResolveAgentKey([]string{"PATH=/bin"}, "aws-bedrock")
	if !ok || !strings.HasSuffix(kv, "=minted-1") {
		t.Fatalf("ResolveAgentKey = %q, %v", kv, ok)
	}
}

func TestProjectAgentEnvFailedCommandStaysUnbound(t *testing.T) {
	writeSecretsMap(t, "AWS_BEDROCK_API_KEY=!bashy bedrock-key\n")
	stubBindingCmd(t, errors.New("no AWS credentials"))
	if kv, ok := ResolveAgentKey([]string{"PATH=/bin"}, "aws-bedrock"); ok {
		t.Fatalf("a failed command must not bind: %q", kv)
	}
}

// `secret env` prints the minted value but never writes it to the cache,
// and still mints when the vault is unreachable.
func TestEnvMintsButNeverCaches(t *testing.T) {
	for _, vaultUp := range []bool{true, false} {
		writeSecretsMap(t, "AWS_BEDROCK_API_KEY=!bashy bedrock-key\nZAI_API_KEY=@zai\n")
		t.Setenv("BASHY_SECRETS_TOKEN", "fixture-token")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !vaultUp {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"secrets":[{"name":"zai","value":"vault-fixture"}]}`))
		}))
		t.Setenv("BASHY_CLOUDBOX_URL", server.URL)
		stubBindingCmd(t, nil, "minted-token")
		var out, errOut bytes.Buffer
		err := runEnv(&out, &errOut, Config{}, "", false)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "export AWS_BEDROCK_API_KEY='minted-token'") {
			t.Fatalf("vaultUp=%v out = %q", vaultUp, out.String())
		}
		if data, _ := os.ReadFile(cacheFile()); strings.Contains(string(data), "minted-token") {
			t.Fatalf("minted value cached: %q", data)
		}
	}
}

func TestRunBindingCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX echo/true")
	}
	if v, err := runBindingCmd([]string{"echo", " token "}, io.Discard); err != nil || v != "token" {
		t.Fatalf("echo = %q, %v", v, err)
	}
	if _, err := runBindingCmd([]string{"true"}, io.Discard); err == nil {
		t.Fatal("empty output must be an error")
	}
	if _, err := runBindingCmd([]string{"false"}, io.Discard); err == nil {
		t.Fatal("a failing command must be an error")
	}
}
