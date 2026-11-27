package cligw

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/gateway"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

func loginTool(hint, checkExec string) fleet.Tool {
	return fleet.Tool{Name: "fakecli", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
		Binary: "fakecli",
		Launch: fleet.ToolLaunch{
			AuthHint: hint,
			Auth:     fleet.ToolAuth{CheckExec: checkExec},
		},
	}}
}

func TestClassifyNeedsLogin(t *testing.T) {
	tool := loginTool("Run `fakecli login` once.", "login")
	for _, output := range []string{
		"Not logged in · Please run /login",
		`{"loggedIn": false}`,
		"session expired, please re-authenticate",
		"Open this page to sign in\nWaiting for approval…",
	} {
		if _, ok := classifyNeedsLogin(tool, "agent-a", output, gateway.UpstreamFailure{}, false); !ok {
			t.Errorf("output %q not recognised as needs-login", output)
		}
	}
	// An explicit provider 401 for a subscription tool is a login failure.
	auth := gateway.UpstreamFailure{Class: gateway.UpstreamAuth, Status: 401}
	if _, ok := classifyNeedsLogin(tool, "agent-a", "boom", auth, true); !ok {
		t.Error("classified 401 not recognised as needs-login")
	}
	// A plain crash is not a login failure.
	if _, ok := classifyNeedsLogin(tool, "agent-a", "fake backend crash", gateway.UpstreamFailure{}, false); ok {
		t.Error("crash misclassified as needs-login")
	}
	// A tool with no declared auth check keeps the old classification.
	bare := loginTool("", "")
	bare.CLI.Launch.Auth.CheckExec = ""
	if _, ok := classifyNeedsLogin(bare, "agent-a", "Not logged in", gateway.UpstreamFailure{}, false); ok {
		t.Error("tool without declared auth check misclassified as needs-login")
	}
}

func TestLoginCommandDerivation(t *testing.T) {
	cases := []struct{ hint, check, want string }{
		{"Run `fakecli login` once.", "", "fakecli login"},
		{"needs a sign-in (`fakecli login`, or a key)", "", "fakecli login"},
		{"Run `fakecli` interactively and sign in.", "", "fakecli"},
		{"no command quoted here", "login", "fakecli login"},
		{"no command quoted here", "auth status --json", "fakecli"},
		{"", "", "fakecli"},
	}
	for _, tc := range cases {
		if got := loginCommand(loginTool(tc.hint, tc.check)); got != tc.want {
			t.Errorf("loginCommand(hint=%q, check=%q) = %q, want %q", tc.hint, tc.check, got, tc.want)
		}
	}
}

func TestNeedsLoginFailureShape(t *testing.T) {
	nl := NeedsLogin{Agent: "agent-a", Tool: "fakecli", LoginCommand: "fakecli login"}
	f := nl.Failure()
	if f.Class != gateway.UpstreamClass("needs_login") || f.Retryable || f.Status != http.StatusUnauthorized {
		t.Fatalf("failure = %+v, want needs_login/401/not-retryable", f)
	}
	if !strings.Contains(f.Hint, "needs-login") || !strings.Contains(f.Hint, "`fakecli login`") {
		t.Fatalf("hint does not name the human step: %q", f.Hint)
	}
	if _, ok := NeedsLoginFailure(&NeedsLoginError{NeedsLogin: nl}); !ok {
		t.Fatal("NeedsLoginFailure did not extract the verdict")
	}
}

// One server whose only binding is a subscription CLI that starts signed
// out: needs-login, no breaker trip, owner notified once; after the
// (simulated) sign-in the same server serves again with no restart.
func TestServerNeedsLoginNoBreakerTripAndRecovery(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLIGW_TEST_HELPER", "1")
	flip := filepath.Join(t.TempDir(), "login-state")
	if err := os.WriteFile(flip, []byte("out"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIGW_FLIP_FILE", flip)

	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	tool := fleet.Tool{Name: "fakelogin", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
		Binary: os.Args[0],
		Launch: fleet.ToolLaunch{
			Exec:         fmt.Sprintf("%s -test.run=TestCLIHelper -- login-flip {model} {prompt}", os.Args[0]),
			Warm:         string(WarmCold),
			EventsStdout: "--events-json",
			EventsDone:   fleet.EventsDone{Field: "type", Values: []string{"result", "turn.completed"}},
			AuthHint:     "Sign in once with the vendor login command.",
			Auth:         fleet.ToolAuth{CheckExec: "login"},
		},
	}}
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "login-model", Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindSubscription, Provider: "fake", Domain: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "login-agent", Tool: "fakelogin", Model: "login-model"}); err != nil {
		t.Fatal(err)
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })

	breaker := sched.NewBreaker()
	var notified atomic.Int32
	server, err := NewServer(ServerOptions{
		Catalog: NewFleetCatalog(cat),
		Quota:   &fakeQuota{headroom: map[string]float64{"login-model": .9}, refused: map[string]string{}},
		Breaker: breaker,
		Pool:    PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
		OnNeedsLogin: func(NeedsLogin) { notified.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	ts := &testServer{Server: server, http: httpServer}

	post := func() (int, string) {
		resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "login-agent"))
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	for i := 1; i <= 2; i++ {
		status, body := post()
		if status != http.StatusUnauthorized {
			t.Fatalf("signed-out request %d = %d, want 401: %s", i, status, body)
		}
		if !strings.Contains(body, "needs_login") || !strings.Contains(body, "needs-login") || !strings.Contains(body, "login") {
			t.Fatalf("signed-out body does not name the sign-in step: %s", body)
		}
		if strings.Contains(body, "transient") || strings.Contains(body, "failing over") {
			t.Fatalf("signed-out body still reads transient: %s", body)
		}
	}
	if breaker.InCooldown("login-agent") {
		t.Fatal("needs-login tripped the breaker")
	}
	if got := notified.Load(); got != 1 {
		t.Fatalf("owner notified %d times, want exactly 1", got)
	}
	nl, ok := server.NeedsLogin("login-agent")
	if !ok || nl.Tool != "fakelogin" || !strings.Contains(nl.LoginCommand, "login") {
		t.Fatalf("needs-login state = %+v, %v", nl, ok)
	}
	found := false
	for _, agent := range server.Health().Agents {
		if agent.Agent == "login-agent" {
			found = agent.NeedsLogin && strings.Contains(agent.LoginCommand, "login") && !agent.Cooling
		}
	}
	if !found {
		t.Fatalf("health does not mark login-agent needs-login: %+v", server.Health().Agents)
	}

	// The operator signs in once; the same door serves again, no restart.
	if err := os.WriteFile(flip, []byte("in"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body := post()
	if status != http.StatusOK || !strings.Contains(body, "hello from cli") {
		t.Fatalf("after login = %d: %s", status, body)
	}
	if _, ok := server.NeedsLogin("login-agent"); ok {
		t.Fatal("needs-login state not cleared after a successful request")
	}
	if breaker.InCooldown("login-agent") {
		t.Fatal("breaker tripped across the needs-login episode")
	}
	if got := notified.Load(); got != 1 {
		t.Fatalf("owner notified %d times after recovery, want 1", got)
	}
}
