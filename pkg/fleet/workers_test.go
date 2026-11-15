package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSubscriptionWorkerYAML covers each vendor's declared adapter contract:
// the check argv, success matchers, lane pinning and config dir env. It
// reads the shipped baseline assets, so a YAML edit that breaks the
// delegate or workers path fails here. No live sign-in: only declarations
// and pure classification are asserted.
func TestSubscriptionWorkerYAML(t *testing.T) {
	c := New()
	for _, tc := range []struct {
		name       string
		checkExec  string
		configDir  string
		tokenRef   string
		laneSubstr []string
		okSample   string
		badSample  string
		badExit    int
	}{
		{
			name:      "claude",
			checkExec: "auth status --json",
			configDir: "CLAUDE_CONFIG_DIR",
			tokenRef:  "claude-code-oauth-token",
			// The lane pins the subscription lane: no settings load,
			// claudeai login method, empty key helper.
			laneSubstr: []string{"--setting-sources", "forceLoginMethod", "apiKeyHelper"},
			okSample:   `{"loggedIn":true,"authMethod":"oauth_token","subscriptionType":"max"}`,
			// An API-key login reports a different method and no plan.
			badSample: `{"loggedIn":true,"authMethod":"api_key","subscriptionType":""}`,
		},
		{
			name:      "codex",
			checkExec: "login status",
			configDir: "CODEX_HOME",
			laneSubstr: []string{
				`forced_login_method="chatgpt"`,
				`model_provider="openai"`,
			},
			okSample:  "Logged in using ChatGPT\n",
			badSample: "Logged in using an API key\n",
		},
		{
			name:      "agy",
			checkExec: "models",
			configDir: "AGY_CONFIG_DIR",
			// One model row: tab-separated id and display name.
			okSample:  "gemini-1-test-medium\tGemini Test (Medium)\n",
			badSample: "not logged in\n",
		},
		{
			name:      "muse",
			checkExec: "login",
			configDir: "MUSE_CONFIG_DIR",
			// Empty ok_match on purpose: the exit code decides (see the
			// asset comment). A signed-out CLI prints the device prompt
			// and fails the run.
			okSample:  "",
			badSample: "Open this page to sign in:\n  https://auth.meta.com/oauth/device/?code=XXXX-XXXX\n\nWaiting for approval…\n",
			badExit:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := c.Tool(tc.name)
			if !ok {
				t.Fatalf("tool %q not in catalog", tc.name)
			}
			auth := tool.CLI.Launch.Auth
			if auth.CheckExec != tc.checkExec {
				t.Fatalf("check_exec = %q, want %q", auth.CheckExec, tc.checkExec)
			}
			if tool.CLI.Launch.ConfigDirEnv != tc.configDir {
				t.Fatalf("config_dir_env = %q, want %q", tool.CLI.Launch.ConfigDirEnv, tc.configDir)
			}
			if auth.TokenRef != tc.tokenRef {
				t.Fatalf("token_ref = %q, want %q", auth.TokenRef, tc.tokenRef)
			}
			if err := tool.ValidateAuth(); err != nil {
				t.Fatalf("ValidateAuth: %v", err)
			}
			if len(AuthCheckArgv(tool)) == 0 {
				t.Fatal("AuthCheckArgv is empty")
			}
			for _, sub := range tc.laneSubstr {
				found := false
				for _, a := range auth.LaneArgs {
					if strings.Contains(a, sub) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("lane_args %q lacks %q", auth.LaneArgs, sub)
				}
			}
			if !CheckAuthOutput(tool, 0, tc.okSample) {
				t.Fatalf("ok sample rejected: %q", tc.okSample)
			}
			if CheckAuthOutput(tool, tc.badExit, tc.badSample) {
				t.Fatalf("bad sample accepted (exit %d): %q", tc.badExit, tc.badSample)
			}
			if CheckAuthOutput(tool, 1, tc.okSample) {
				t.Fatal("nonzero exit accepted")
			}
		})
	}
}

// TestSubscriptionWorkerLaneParity pins the exact probe argv each vendor
// used to hard-code in chat/delegate_guard.go: the lane args must precede
// the check fragment in order, so the adapter move changed no byte.
func TestSubscriptionWorkerLaneParity(t *testing.T) {
	c := New()
	claude, _ := c.Tool("claude")
	if got, want := AuthCheckArgv(claude), []string{
		"--setting-sources", "",
		"--settings", `{"forceLoginMethod":"claudeai","apiKeyHelper":""}`,
		"auth", "status", "--json",
	}; !equalStrings(got, want) {
		t.Fatalf("claude probe argv = %q, want %q", got, want)
	}
	codex, _ := c.Tool("codex")
	if got, want := AuthCheckArgv(codex), []string{
		"-c", `forced_login_method="chatgpt"`,
		"-c", `model_provider="openai"`,
		"login", "status",
	}; !equalStrings(got, want) {
		t.Fatalf("codex probe argv = %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClassifyWorker covers the state machine: ok, missing, expired,
// plan-limit, offline. The wire contract uses the same reasons when a
// worker cannot run.
func TestClassifyWorker(t *testing.T) {
	tool := Tool{Name: "fixture"}
	tool.CLI.Launch.Auth.OkMatch = []string{`Logged in using ChatGPT`}
	tool.UsageSignals = []UsageSignal{{ID: "limit", Match: `(?i)hit your usage limit`, Kind: "exhausted"}}
	for _, tc := range []struct {
		name string
		res  WorkerCheck
		want WorkerState
	}{
		{"ok", WorkerCheck{Output: "Logged in using ChatGPT\n"}, WorkerOK},
		{"missing exit", WorkerCheck{ExitCode: 1, Output: "not logged in\n"}, WorkerMissing},
		{"missing json", WorkerCheck{Output: `{"loggedIn":false}`}, WorkerMissing},
		{"missing device prompt", WorkerCheck{TimedOut: true, ExitCode: 124, Output: "Open this page to sign in:\nWaiting for approval…\n"}, WorkerMissing},
		{"expired token", WorkerCheck{ExitCode: 1, Output: "error: token expired, please log in again\n"}, WorkerExpired},
		{"expired refresh", WorkerCheck{ExitCode: 1, Output: "failed to refresh credentials\n"}, WorkerExpired},
		{"usage signal", WorkerCheck{ExitCode: 1, Output: "You've hit your usage limit\n"}, WorkerPlanLimit},
		{"generic limit", WorkerCheck{ExitCode: 1, Output: "rate limit exceeded, resets soon\n"}, WorkerPlanLimit},
		{"limit beats expired", WorkerCheck{ExitCode: 1, Output: "token expired; weekly quota exhausted\n"}, WorkerPlanLimit},
		{"no binary", WorkerCheck{MissingBinary: true, Output: "not found"}, WorkerOffline},
		{"hung probe", WorkerCheck{TimedOut: true, ExitCode: 124}, WorkerOffline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyWorker(tool, tc.res); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrepareWorkerEnvClaude resolves the subscription token from the
// parent environment (no vault involved) and isolates the config dir.
func TestPrepareWorkerEnvClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	c := New()
	tool, ok := c.Tool("claude")
	if !ok {
		t.Fatal("claude not in catalog")
	}
	parent := []string{"HOME=" + home, "CLAUDE_CODE_OAUTH_TOKEN=fake-token"}
	got, err := PrepareWorkerEnv(parent, []string{"PATH=/usr/bin"}, tool)
	if err != nil {
		t.Fatalf("PrepareWorkerEnv: %v", err)
	}
	wantDir := filepath.Join(home, ".bashy", "workers", "claude", "config")
	assertEnv(t, got, "CLAUDE_CONFIG_DIR="+wantDir)
	assertEnv(t, got, "CLAUDE_CODE_OAUTH_TOKEN=fake-token")
	if st, err := os.Stat(wantDir); err != nil || !st.IsDir() {
		t.Fatalf("worker dir missing: %v", err)
	}
}

// TestPrepareWorkerEnvTokenMissing fails closed: an unresolvable vault ref
// refuses the worker instead of falling back to an API key.
func TestPrepareWorkerEnvTokenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// An empty template dir: no host binding can resolve the ref, so no
	// vault is contacted.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tool := Tool{Name: "claude"}
	tool.CLI.Launch.ConfigDirEnv = "CLAUDE_CONFIG_DIR"
	tool.CLI.Launch.Auth.TokenRef = "test-nonexistent-ref-xyz"
	if _, err := PrepareWorkerEnv([]string{"HOME=" + home}, nil, tool); err == nil {
		t.Fatal("expected missing-credential error, got nil")
	}
}

// TestPrepareWorkerEnvCodex seeds the device-code cache from the operator's
// config source into the worker dir without touching the operator's files.
func TestPrepareWorkerEnvCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(home, "srccodex")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{"kind":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", src)
	c := New()
	tool, ok := c.Tool("codex")
	if !ok {
		t.Fatal("codex not in catalog")
	}
	got, err := PrepareWorkerEnv([]string{"HOME=" + home, "CODEX_HOME=" + src}, []string{"PATH=/usr/bin"}, tool)
	if err != nil {
		t.Fatalf("PrepareWorkerEnv: %v", err)
	}
	wantDir := filepath.Join(home, ".bashy", "workers", "codex", "config")
	assertEnv(t, got, "CODEX_HOME="+wantDir)
	b, err := os.ReadFile(filepath.Join(wantDir, "auth.json"))
	if err != nil || string(b) != `{"kind":"chatgpt"}` {
		t.Fatalf("seeded auth.json = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(src, "auth.json")); err != nil {
		t.Fatalf("operator source disturbed: %v", err)
	}
}

// TestPrepareWorkerEnvMarkerRedirect covers the marker branch: the vendor
// reads through a base variable (HOME for agy, XDG_CONFIG_HOME for muse),
// so the base moves to the worker home and the marker names the seeded dir.
func TestPrepareWorkerEnvMarkerRedirect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	museSrc := filepath.Join(home, "srcconfig", "muse")
	if err := os.MkdirAll(museSrc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(museSrc, "auth.json"), []byte(`{"kind":"subscription"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "srcconfig"))
	c := New()
	tool, ok := c.Tool("muse")
	if !ok {
		t.Fatal("muse not in catalog")
	}
	got, err := PrepareWorkerEnv(os.Environ(), []string{"PATH=/usr/bin"}, tool)
	if err != nil {
		t.Fatalf("PrepareWorkerEnv: %v", err)
	}
	wantBase := filepath.Join(home, ".bashy", "workers", "muse", "config")
	assertEnv(t, got, "XDG_CONFIG_HOME="+wantBase)
	assertEnv(t, got, "MUSE_CONFIG_DIR="+filepath.Join(wantBase, "muse"))
	b, err := os.ReadFile(filepath.Join(wantBase, "muse", "auth.json"))
	if err != nil || string(b) != `{"kind":"subscription"}` {
		t.Fatalf("seeded auth.json = %q, %v", b, err)
	}
}

// TestProbeWorkerOffline reports a missing binary as offline without running
// anything.
func TestProbeWorkerOffline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tool := Tool{Name: "no-such-vendor-binary-xyz"}
	tool.CLI.Binary = "no-such-vendor-binary-xyz"
	tool.CLI.Launch.Auth.CheckExec = "login status"
	rep := ProbeWorker(context.Background(), tool, 5*time.Second)
	if rep.State != WorkerOffline {
		t.Fatalf("state = %q, want offline", rep.State)
	}
}

// TestProbeWorkerLive exercises the full probe path against a fake vendor
// CLI on PATH: scrubbed env, worker config dir, check argv, classification.
func TestProbeWorkerLive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	dir := t.TempDir()
	fake := "#!/bin/sh\n" +
		"case \"$1\" in models) printf 'gemini-1-test-medium\\tGemini Test (Medium)\\n'; exit 0;; esac\n" +
		"echo 'not logged in'; exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := New()
	tool, ok := c.Tool("agy")
	if !ok {
		t.Fatal("agy not in catalog")
	}
	tool.CLI.Binary = "agy"
	reps := ProbeWorkers(context.Background(), c, []string{"agy", "no-such-tool"}, 10*time.Second)
	if len(reps) != 2 {
		t.Fatalf("reports = %d, want 2", len(reps))
	}
	if reps[0].State != WorkerOK {
		t.Fatalf("agy state = %q (%s), want ok", reps[0].State, reps[0].Detail)
	}
	if reps[1].State != WorkerOffline {
		t.Fatalf("unknown state = %q, want offline", reps[1].State)
	}
}

func assertEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, kv := range env {
		if kv == want {
			return
		}
	}
	t.Fatalf("env lacks %q (got %q)", want, env)
}

// fakeWorkerBin stages a fake vendor CLI speaking the tool's real check
// protocol. ok=false makes the check fail like a signed-out CLI.
func fakeWorkerBin(t *testing.T, dir, name, vendor string, ok bool) {
	t.Helper()
	body := "#!/bin/sh\n"
	switch vendor {
	case "agy":
		if ok {
			body += "printf 'gemini-1-test-medium\\tGemini Test (Medium)\\n'; exit 0\n"
		} else {
			body += "echo 'not logged in'; exit 1\n"
		}
	case "codex":
		if ok {
			body += "echo 'Logged in using ChatGPT'; exit 0\n"
		} else {
			body += "echo 'not logged in'; exit 1\n"
		}
	default:
		t.Fatalf("unsupported fake vendor %q", vendor)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestWorkersCmdJSON runs the `workers` verb against fake CLIs and checks
// the --json envelope carries every state.
func TestWorkersCmdJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir := t.TempDir()
	// Two fake tools with distinct binaries but real-shaped checks: one
	// signed in (agy protocol), one signed out (codex protocol).
	fakeWorkerBin(t, dir, "w-ok", "agy", true)
	fakeWorkerBin(t, dir, "w-bad", "codex", false)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	c := New(WithRoot(root))
	agy, _ := c.Tool("agy")
	agy.Name, agy.CLI.Binary = "w-ok", "w-ok"
	if err := c.SaveTool(agy); err != nil {
		t.Fatal(err)
	}
	codex, _ := c.Tool("codex")
	codex.Name, codex.CLI.Binary = "w-bad", "w-bad"
	if err := c.SaveTool(codex); err != nil {
		t.Fatal(err)
	}
	cmd := newWorkersCmd([]Option{WithRoot(root)})
	cmd.SetArgs([]string{"--json", "w-ok", "w-bad", "w-ghost"})
	var out strings.Builder
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workers: %v", err)
	}
	var reps []WorkerReport
	if err := json.Unmarshal([]byte(out.String()), &reps); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	want := map[string]WorkerState{"w-ok": WorkerOK, "w-bad": WorkerMissing, "w-ghost": WorkerOffline}
	if len(reps) != len(want) {
		t.Fatalf("reports = %+v, want 3", reps)
	}
	for _, r := range reps {
		if want[r.Name] != r.State {
			t.Errorf("%s state = %q, want %q (%s)", r.Name, r.State, want[r.Name], r.Detail)
		}
		if r.Detail == "" {
			t.Errorf("%s has no detail", r.Name)
		}
	}
}

// TestWorkersProbeAlerts checks daily-probe mode: non-ok workers are
// recorded to the bounded alert log, announced on stderr, and fail the run.
func TestWorkersProbeAlerts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir := t.TempDir()
	fakeWorkerBin(t, dir, "w-ok", "agy", true)
	fakeWorkerBin(t, dir, "w-bad", "codex", false)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	c := New(WithRoot(root))
	agy, _ := c.Tool("agy")
	agy.Name, agy.CLI.Binary = "w-ok", "w-ok"
	if err := c.SaveTool(agy); err != nil {
		t.Fatal(err)
	}
	codex, _ := c.Tool("codex")
	codex.Name, codex.CLI.Binary = "w-bad", "w-bad"
	if err := c.SaveTool(codex); err != nil {
		t.Fatal(err)
	}
	cmd := newWorkersCmd([]Option{WithRoot(root)})
	cmd.SetArgs([]string{"--json", "--probe", "--probe-timeout", "5s", "w-ok", "w-bad"})
	var out, errs strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errs)
	if err := cmd.Execute(); err == nil {
		t.Fatal("probe with a missing worker must fail")
	}
	if !strings.Contains(errs.String(), "ALERT worker=w-bad state=missing") {
		t.Fatalf("no alert on stderr: %q", errs.String())
	}
	if strings.Contains(errs.String(), "ALERT worker=w-ok") {
		t.Fatalf("ok worker alerted: %q", errs.String())
	}
	log, err := os.ReadFile(filepath.Join(home, ".bashy", "workers", "alerts.jsonl"))
	if err != nil {
		t.Fatalf("alert log: %v", err)
	}
	if !strings.Contains(string(log), `"worker":"w-bad"`) || strings.Contains(string(log), `"worker":"w-ok"`) {
		t.Fatalf("alert log = %s", log)
	}
	if strings.Contains(string(log), "auth.json") {
		t.Fatal("alert log must never carry credential material")
	}
}
