package cligw

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("llm %s: %v (output: %s)", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestEnvCommandPrintsTheClientVariables(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	token, err := LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	got := runCmd(t, "env")

	want := []string{
		fmt.Sprintf("OPENAI_BASE_URL=http://127.0.0.1:%d/v1", DefaultPort),
		"OPENAI_API_KEY=" + token,
		fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d/anthropic", DefaultPort),
		"ANTHROPIC_API_KEY=" + token,
		"export OPENAI_BASE_URL OPENAI_API_KEY ANTHROPIC_BASE_URL ANTHROPIC_API_KEY",
	}
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Fatalf("llm env output missing %q:\n%s", line, got)
		}
	}
	// It must be eval-able: every line is an assignment, an export or a
	// comment, and nothing else.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "export ") || strings.Contains(line, "=") {
			continue
		}
		t.Fatalf("llm env emitted a line eval cannot swallow: %q", line)
	}
}

func TestPoolsCommandRendersARunningServer(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .8, "gpt-x": .2})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// The quota headroom and the rate estimates are the SCALER's view, and
	// it forms them on a tick. `llm serve` runs that ticker; a test that
	// does not must drive one step itself or it reads a pre-tick snapshot.
	ts.Autoscaler().Tick()

	// Point the endpoint record at the httptest server, which is what a
	// `llm serve` in another terminal would have written.
	if err := WriteEndpoint(Endpoint{
		SchemaVersion: EndpointSchemaVersion, BaseURL: ts.http.URL, Bind: BindLoopback, Anthropic: true,
	}); err != nil {
		t.Fatal(err)
	}

	table := runCmd(t, "pools")
	for _, want := range []string{"status ok", "AGENT", "HEADROOM", "warm-four", "L4", "BAND", "PREWARM"} {
		if !strings.Contains(table, want) {
			t.Fatalf("llm pools output missing %q:\n%s", want, table)
		}
	}
	if !strings.Contains(table, "80%") {
		t.Fatalf("llm pools did not render the seat headroom:\n%s", table)
	}

	asJSON := runCmd(t, "pools", "--json")
	for _, want := range []string{HealthSchemaVersion, AutoscaleSchemaVersion, `"agent": "warm-four"`} {
		if !strings.Contains(asJSON, want) {
			t.Fatalf("llm pools --json missing %q:\n%s", want, asJSON)
		}
	}
}

func TestPoolsCommandWithNoServerSaysSo(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// Port 1 is not ours and nothing is serving there: the error must name
	// the verb that would start one.
	if err := WriteEndpoint(Endpoint{SchemaVersion: EndpointSchemaVersion, BaseURL: "http://127.0.0.1:1", Bind: BindLoopback}); err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"pools"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("pools against a dead endpoint must fail")
	}
	if !strings.Contains(err.Error(), "llm serve") {
		t.Fatalf("error does not point at the fix: %v", err)
	}
}

func TestCommandTreeIsTheDocumentedSurface(t *testing.T) {
	cmd := NewCmd()
	if cmd.Name() != "llm" {
		t.Fatalf("command name = %q", cmd.Name())
	}
	got := map[string]*bytes.Buffer{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = nil
		if sub.Short == "" {
			t.Fatalf("%s has no one-line help", sub.Name())
		}
	}
	for _, want := range []string{"serve", "pools", "env"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("llm is missing the %q subcommand: %v", want, got)
		}
	}
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"bind", "port", "policy", "prewarm"} {
		if serve.Flags().Lookup(flag) == nil {
			t.Fatalf("serve has no --%s flag", flag)
		}
	}
	if def := serve.Flags().Lookup("bind").DefValue; def != BindLoopback {
		t.Fatalf("--bind defaults to %q, want loopback: a gateway that spends the owner's seats must not bind wider by default", def)
	}
	if def := serve.Flags().Lookup("port").DefValue; def != fmt.Sprint(DefaultPort) {
		t.Fatalf("--port defaults to %q, want %d", def, DefaultPort)
	}
}
