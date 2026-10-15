package snippet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWireForms(t *testing.T) {
	for _, raw := range []string{`"echo hi"`, `{"lang":"bsh","body":"echo hi","effects":["net"],"env":{"KEY":"secret:deepseek"},"timeout":"20s"}`} {
		var s Snippet
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var again Snippet
		if err := json.Unmarshal(b, &again); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s, again) {
			t.Fatalf("%+v != %+v", s, again)
		}
	}
	for _, raw := range []string{"run: echo hi\n", "run: {lang: bsh, body: echo hi, effects: [net], env: {KEY: 'secret:deepseek'}, timeout: 20s}\n"} {
		var v struct {
			Run Snippet `yaml:"run"`
		}
		if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var again struct {
			Run Snippet `yaml:"run"`
		}
		if err := yaml.Unmarshal(b, &again); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, again) {
			t.Fatalf("%+v != %+v", v, again)
		}
	}
}

func TestRunValidationAndResolution(t *testing.T) {
	SetRunner(func(_ context.Context, _ Snippet, tio IO) (Result, error) {
		return Result{Stdout: []byte("visible"), Stderr: []byte(strings.Join(tio.Env, "\n")), Exit: 0}, errors.New("bad secret-value")
	})
	defer SetRunner(nil)
	SetSecretResolver(func(_ context.Context, ref string) (string, error) {
		if ref != "deepseek" {
			t.Fatal(ref)
		}
		return "secret-value", nil
	})
	defer SetSecretResolver(nil)
	SetModelCredentialResolver(func(_ context.Context, m ModelRef) (string, error) {
		if m.Provider != "p" || m.Model != "m" || m.Plan != "plan" {
			t.Fatal(m)
		}
		return "model-value", nil
	})
	defer SetModelCredentialResolver(nil)
	t.Setenv("SNIPPET_TEST_KEY", "env-value")
	for _, s := range []Snippet{{Lang: "no", Body: "echo"}, {Body: "echo", Effects: []string{"bad"}}, {Body: "echo", Env: map[string]string{"KEY": "literal"}}, {Body: "cat auth.json", Effects: []string{"read"}}} {
		if _, err := Run(context.Background(), s, IO{}); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	s := Snippet{Body: "echo", Effects: []string{"pure"}, Env: map[string]string{"A": "secret:deepseek", "B": "env:SNIPPET_TEST_KEY", "C": "model:credential"}}
	r, err := Run(WithModel(context.Background(), "p", "m", "plan"), s, IO{})
	if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(string(r.Stderr), "secret-value") {
		t.Fatalf("err=%v stderr=%s", err, r.Stderr)
	}
	if !strings.Contains(string(r.Stderr), "A=***") || !strings.Contains(string(r.Stderr), "B=***") || !strings.Contains(string(r.Stderr), "C=***") {
		t.Fatal(string(r.Stderr))
	}
	if string(r.Stdout) != "visible" {
		t.Fatal(string(r.Stdout))
	}
	_, err = Run(context.Background(), Snippet{Body: "echo", Env: map[string]string{"C": "model:credential"}}, IO{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err=%v", err)
	}
}

func TestConsentAndCredentialPaths(t *testing.T) {
	for _, tc := range []struct {
		effects []string
		want    bool
	}{{nil, false}, {[]string{"pure", "read"}, false}, {[]string{"net"}, true}, {[]string{"write"}, true}} {
		if got := NeedsConsent(Snippet{Effects: tc.effects}); got != tc.want {
			t.Fatalf("%v: %v", tc.effects, got)
		}
	}
	for _, p := range []string{"auth.json", ".credentials.json", "oauth_creds.json", "credentials.json", "Library/Keychains", ".netrc"} {
		if _, ok := CredentialPathRefused("cat " + p); !ok {
			t.Fatal(p)
		}
	}
	if _, ok := CredentialPathRefused("echo safe"); ok {
		t.Fatal("false refusal")
	}
	_ = time.Second
}
