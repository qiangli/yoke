package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
)

// fakeDelegateVendor stages a fake vendor CLI on PATH and runs the delegate
// flow against the REAL embedded tool asset for the four subscription
// workers (claude, codex, agy, muse), so the test covers each vendor's
// declared auth.check_exec, ok_match, lane_args and config_dir_env. ycode
// keeps its synthetic override (its real exec is `bashy genie …`, which
// would recurse instead of running the fake).
//
// The fake speaks the vendor's real check protocol and asserts the real
// lane args (claude/codex) or the real worker environment (agy/muse, and
// the config-dir half of claude/codex) on the task. HOME, XDG_CONFIG_HOME
// and CODEX_HOME point at temp dirs, so no operator config, vault template
// or cache is ever touched; the claude token is a fake parent-env value
// that ResolveAgentKey finds without reaching a vault.
func fakeDelegateVendor(t *testing.T, vendor string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	pinCatalog(t)
	isolateRoom(t)
	permitUnsafeLaunch(t)
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	t.Setenv("BASHY_NO_COACH", "1")
	t.Setenv("BASHY_DELEGATE_CALLER", "genie")
	t.Setenv("BASHY_DELEGATE_ALLOW", vendor+":test-model")
	t.Setenv("BASHY_ATTEST_ID", "receipt-test")
	t.Setenv("BASHY_LADDER_STORE", filepath.Join(t.TempDir(), "events.jsonl"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if vendor == "claude" {
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-test-oauth-token")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	t.Setenv("DELEGATE_TEST_MARKER", marker)
	script := `#!/bin/sh
VENDOR="` + vendor + `"
for name in ANTHROPIC_API_KEY OPENAI_API_KEY GEMINI_API_KEY GOOGLE_API_KEY DEEPSEEK_API_KEY ANTHROPIC_AUTH_TOKEN AWS_ACCESS_KEY_ID; do
 eval 'value=${'"$name"'-}'
 if [ -n "$value" ]; then echo "metered credential leaked"; exit 19; fi
done
probe_ok() {
 case "$DELEGATE_TEST_MODE" in
  auth) echo 'not logged in'; exit 1;;
  metered) echo 'Logged in using an API key'; exit 0;;
  authhang) sleep 10; exit 0;;
  startfail) printf '#!/nonexistent/delegate-test-interpreter\n' > "$0";;
 esac
}
case "$VENDOR" in
 claude)
  case "$*" in *forceLoginMethod*) ;; *) echo 'missing subscription guard'; exit 18;; esac
  case "$*" in
   *"auth status"*) probe_ok; echo '{"loggedIn":true,"authMethod":"oauth_token","subscriptionType":"max"}'; exit 0;;
  esac;;
 codex)
  case "$*" in *forced_login_method*) ;; *) echo 'missing subscription guard'; exit 18;; esac
  case "$*" in
   *"login status"*) probe_ok; echo 'Logged in using ChatGPT' >&2; exit 0;;
  esac;;
 agy)
  case "$1" in
   models) probe_ok; printf 'gemini-1-test-medium\tGemini Test (Medium)\n'; exit 0;;
  esac;;
 muse)
  case "$1" in
   login) probe_ok; exit 0;;
  esac;;
esac
# Task path: the worker environment must carry the per-worker config dir
# (and the subscription token / contributor pin where declared).
case "$VENDOR" in
 claude) [ -n "$CLAUDE_CONFIG_DIR" ] || { echo 'missing worker config dir'; exit 17; }
  [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || { echo 'missing worker token'; exit 17; };;
 codex) [ -n "$CODEX_HOME" ] || { echo 'missing worker config dir'; exit 17; };;
 agy) [ -n "$AGY_CONFIG_DIR" ] || { echo 'missing worker config dir'; exit 17; };;
 muse) [ -n "$MUSE_CONFIG_DIR" ] || { echo 'missing worker config dir'; exit 17; }
  [ "$MUSE_CONTRIBUTOR_MODE" = 0 ] || { echo 'contributor mode not pinned off'; exit 17; };;
esac
printf task > "$DELEGATE_TEST_MARKER"
case "$DELEGATE_TEST_MODE" in
 idle|timeout) sleep 10;;
 heartbeat) for n in 1 2 3 4 5; do echo tick >&2; sleep 0.04; done;;
 vendor) echo failed >&2; exit 7;;
esac
echo delegated
`
	bin := filepath.Join(dir, vendor)
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cat := newCatalog()
	if vendor == "ycode" {
		if err := cat.SaveTool(fleet.Tool{Name: vendor, Aliases: aliasesFor(vendor), Builtin: true, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: vendor, Launch: fleet.ToolLaunch{Exec: vendor + " --model {model} {prompt}", Env: []string{"OPENAI_API_KEY=recipe-secret"}}}}); err != nil {
			t.Fatal(err)
		}
		return marker
	}
	// The real embedded asset, minus its managed pin: binmgr would otherwise
	// resolve (or install) the vendor's release instead of the PATH fake.
	// Every declared field under test — auth, lane_args, config_dir_env,
	// env — stays exactly as shipped.
	tool, ok := cat.Tool(vendor)
	if !ok {
		t.Fatalf("embedded tool %q not found", vendor)
	}
	tool.CLI.Managed = nil
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestDelegateVendorContract(t *testing.T) {
	for _, vendor := range []string{"claude", "codex", "agy", "muse"} {
		t.Run(vendor, func(t *testing.T) {
			marker := fakeDelegateVendor(t, vendor)
			for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN", "AWS_ACCESS_KEY_ID"} {
				t.Setenv(name, "parent-secret")
			}
			for _, tc := range []struct {
				name, mode, kind string
				flags            []string
				launched         bool
			}{
				{"success", "", "", nil, true},
				{"vendor", "vendor", "vendor", nil, true},
				{"timeout", "timeout", "timeout", []string{"--timeout=3s"}, true},
				{"idle", "idle", "idle", []string{"--idle-timeout=500ms"}, true},
				{"stderr resets idle", "heartbeat", "", []string{"--idle-timeout=150ms"}, true},
				{"dollars", "", "budget", []string{"--budget-usd=1"}, false},
				{"tokens", "", "budget", []string{"--max-tokens=100"}, false},
				{"zero cap", "", "budget", []string{"--max-tokens=0"}, false},
				{"denied", "", "denied", []string{"--allow=other:model"}, false},
				{"empty genie allow", "", "denied", []string{"--allow="}, false},
				{"auth", "auth", "auth", nil, false},
				{"metered auth", "metered", "auth", nil, false},
				{"auth timeout", "authhang", "timeout", []string{"--timeout=100ms"}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// The probe and task run in a scrubbed environment no
					// API key survives, so a metered muse login cannot
					// happen through this path: the case is meaningless
					// for a tool with no key spelling in its check.
					if vendor == "muse" && tc.mode == "metered" {
						t.Skip("muse has no metered login spelling in a scrubbed probe env")
					}
					_ = os.Remove(marker)
					t.Setenv("BASHY_LADDER_STORE", filepath.Join(t.TempDir(), "events.jsonl"))
					t.Setenv("DELEGATE_TEST_MODE", tc.mode)
					cmd := NewDelegateCmd()
					cmd.SetArgs(append([]string{"--json", vendor + ":test-model", "do work"}, tc.flags...))
					var stdout, stderr bytes.Buffer
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					start := time.Now()
					err := cmd.Execute()
					if time.Since(start) > 5*time.Second {
						t.Fatal("delegation failed to stop promptly")
					}
					var res delegateResult
					if e := json.Unmarshal(stdout.Bytes(), &res); e != nil {
						t.Fatalf("envelope: %v: %s (%v)", e, stdout.String(), err)
					}
					if res.SchemaVersion != "bashy-chat-v1" || res.Receipt != "receipt-test" || res.Identity != "genie-delegate:"+vendor+"-test-model" {
						t.Fatalf("metadata: %+v", res)
					}
					if tc.kind == "" {
						if err != nil || res.Exit != 0 || res.Error != nil || !strings.Contains(res.Output, "delegated") {
							t.Fatalf("success: %+v %v", res, err)
						}
					} else if err == nil || res.Exit == 0 || res.ExitCode != res.Exit || res.Error == nil || res.Error.Kind != tc.kind {
						t.Fatalf("want %s: %+v %v", tc.kind, res, err)
					}
					_, e := os.Stat(marker)
					// The wall deadline also covers admission and authentication, so
					// an overloaded host may legitimately expire it before task launch.
					wantEvents := 0
					if e == nil {
						wantEvents = 1
					}
					outcome := 0.0
					if res.Exit == 0 {
						outcome = 1
					}
					assertDelegateEvents(t, wantEvents, "genie-delegate:"+vendor+"-test-model", outcome)
					if tc.mode != "timeout" && (e == nil) != tc.launched {
						t.Fatalf("task launched=%v want %v", e == nil, tc.launched)
					}
				})
			}
		})
	}
}

func TestDelegateOrdinaryCaller(t *testing.T) {
	fakeDelegateVendor(t, "codex")
	t.Setenv("BASHY_DELEGATE_CALLER", "")
	t.Setenv("BASHY_DELEGATE_ALLOW", "")
	t.Setenv("BASHY_ATTEST_ID", "")
	cmd := NewDelegateCmd()
	cmd.SetArgs([]string{"--json", "codex:test-model", "hi"})
	var b bytes.Buffer
	cmd.SetOut(&b)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var res delegateResult
	if err := json.Unmarshal(b.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	assertDelegateEvents(t, 0, "", 0)
	if res.Identity != "" || res.Receipt != "" || res.Exit != 0 {
		t.Fatalf("ordinary caller: %+v", res)
	}
}

func TestDelegatePolicyUsesResolvedBinding(t *testing.T) {
	fakeDelegateVendor(t, "codex")
	cat := newCatalog()
	if err := cat.SaveAgent(fleet.Agent{Name: "reviewer-alias", Tool: "codex", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		allow  string
		denied bool
	}{
		{"codex:test-model", false},
		{"claude:test-model, codex:test-model", false},
		{"reviewer-alias", true},
		{"codex:*", true},
		{"codex:other", true},
	} {
		t.Run(tc.allow, func(t *testing.T) {
			cmd := NewDelegateCmd()
			cmd.SetArgs([]string{"--json", "--dry-run", "--allow=" + tc.allow, "reviewer-alias", "hi"})
			var b bytes.Buffer
			cmd.SetOut(&b)
			cmd.SetErr(&b)
			err := cmd.Execute()
			assertDelegateEvents(t, 0, "", 0)
			// Cobra may append its diagnostic after the envelope on failure.
			var res delegateResult
			if e := json.NewDecoder(&b).Decode(&res); e != nil {
				t.Fatal(e)
			}
			if tc.denied {
				if err == nil || res.Error == nil || res.Error.Kind != "denied" {
					t.Fatalf("allowed alias bypass: %+v %v", res, err)
				}
			} else if err != nil || res.Identity != "genie-delegate:codex-test-model" {
				t.Fatalf("resolved binding refused: %+v %v", res, err)
			}
		})
	}
}

func TestDelegateAuthPreservesExpiredDeadline(t *testing.T) {
	fakeDelegateVendor(t, "codex")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	ctx = context.WithValue(ctx, delegateKey{}, &delegateGuard{})
	err := delegateAuth(ctx, "codex", t.TempDir(), Launch{ToolName: "codex"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired auth probe: %v", err)
	}
}

func assertDelegateEvents(t *testing.T, count int, identity string, outcome float64) {
	t.Helper()
	st, err := ladder.OpenStore(os.Getenv("BASHY_LADDER_STORE"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("ladder events = %d, want %d: %+v", len(events), count, events)
	}
	for _, ev := range events {
		if ev.Agent != identity || ev.RatingAgent() != identity || ev.Outcome != outcome || ev.Duty != ladder.DutyCode || ev.Points != 1 {
			t.Fatalf("unexpected delegate event: %+v", ev)
		}
	}
}

func TestDelegateStartFailureDoesNotScore(t *testing.T) {
	marker := fakeDelegateVendor(t, "codex")
	t.Setenv("DELEGATE_TEST_MODE", "startfail")
	cmd := NewDelegateCmd()
	cmd.SetArgs([]string{"--json", "codex:test-model", "hi"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected launch failure")
	}
	var res delegateResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Kind != "vendor" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("task marker: %v", err)
	}
	assertDelegateEvents(t, 0, "", 0)
}

func TestDelegateFirstPartyGenie(t *testing.T) {
	marker := fakeDelegateVendor(t, "ycode")
	bin, err := exec.LookPath("ycode")
	if err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\n[ -z \"$OPENAI_API_KEY\" ] || exit 19\nprintf task > \"$DELEGATE_TEST_MARKER\"\necho delegated\n"
	if err := os.WriteFile(bin, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "parent-secret")
	for _, tc := range []struct {
		name, allow, target string
		denied              bool
	}{
		{"ycode spelling", "ycode:test-model", "genie:test-model", false},
		{"genie spelling", "genie:test-model", "genie:test-model", false},
		{"not allowed", "ycode:other", "genie:test-model", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(marker)
			t.Setenv("DELEGATE_TEST_MODE", "auth") // a vendor probe would fail
			cmd := NewDelegateCmd()
			cmd.SetArgs([]string{"--json", "--allow=" + tc.allow, "--agent", tc.target, "do work"})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := cmd.Execute()
			var res delegateResult
			if e := json.Unmarshal(stdout.Bytes(), &res); e != nil {
				t.Fatalf("envelope: %v: %s (%v)", e, stdout.String(), err)
			}
			_, e := os.Stat(marker)
			if tc.denied {
				if err == nil || res.Error == nil || res.Error.Kind != "denied" || e == nil {
					t.Fatalf("want denied: %+v %v", res, err)
				}
				return
			}
			if err != nil || res.Exit != 0 || res.Error != nil || e != nil {
				t.Fatalf("want ok: %+v %v marker=%v", res, err, e)
			}
		})
	}
}

func aliasesFor(vendor string) []string {
	if vendor == "ycode" {
		return []string{"genie"}
	}
	return nil
}
