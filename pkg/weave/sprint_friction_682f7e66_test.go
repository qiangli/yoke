package weave

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/llmbudget"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func runSprintWith(t *testing.T, opts []SprintOption, args ...string) (string, int) {
	t.Helper()
	cmd := NewSprintCmd(opts...)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	combined := out.String() + errBuf.String()
	if err != nil {
		return combined, ExitCode(err)
	}
	return combined, 0
}

// Regression for 682f7e66 (1): `sprint show` must default to cached resource
// observations (unless --probe-resources is passed), memoize HostRoles across
// inbox-pressure scans, and finish well under 10s.
func TestSprintShowDefaultsToCachedSourcesAndCompletesFast(t *testing.T) {
	repo := setupIsolationFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_MB_DIR", filepath.Join(home, ".bashy", "mb"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, ".bashy", "room"))

	seedLiveAgent(t, "mgr-fast")
	t.Setenv("WEAVE_CONDUCTOR", "mgr-fast")
	t.Chdir(repo)

	if out, code := runSprint(t, "add", "fast sprint show"); code != 0 {
		t.Fatalf("sprint add (%d): %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "mgr-fast", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start (%d): %s", code, out)
	}

	// Seed 200 directed board posts (>64 KB) so renderSprintInboxPressure scans
	// the full board during `sprint show`.
	pad := strings.Repeat("x", 400)
	for i := 0; i < 200; i++ {
		to := "worker-a"
		if i%50 == 0 {
			to = "mgr-fast"
		}
		if err := bus.PostMessage(bus.Post{From: "worker-a", To: to, Body: fmt.Sprintf("m-%d-%s", i, pad)}); err != nil {
			t.Fatal(err)
		}
	}

	// Configure an llmbudget gate whose live adapter would sleep 3s per call if invoked.
	var probeCalls atomic.Int32
	gateCfg := llmbudget.Config{
		PolicyPath: filepath.Join(home, "policy.json"),
		StatePath:  filepath.Join(home, "state.json"),
	}
	if err := os.WriteFile(gateCfg.PolicyPath, []byte(`{"version":1,"sources":[{"id":"slow-probe","kind":"slow","provider":"anthropic","account":"a","pool":"a","lane":"subscription","enabled":true,"refresh_seconds":60}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	slowGate := llmbudget.New(gateCfg)
	slowAdapter := slowTestAdapter{calls: &probeCalls}
	gateCfg.Adapters = []llmbudget.Adapter{slowAdapter}
	slowGate = llmbudget.New(gateCfg)

	provider := func(ctx context.Context, id int64) (SprintResourceSummary, error) {
		rep, err := slowGate.CollectReport(ctx, llmbudget.ReportOptions{})
		if err != nil {
			return SprintResourceSummary{}, err
		}
		return SprintResourceSummary{
			At:       time.Now().UTC(),
			Status:   "partial",
			Accounts: len(rep.Accounts),
			Read:     fmt.Sprintf("bashy sprint monitor %d", id),
		}, nil
	}

	start := time.Now()
	out, code := runSprintWith(t, []SprintOption{WithSprintResources(provider)}, "show", "1")
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("sprint show (%d): %s", code, out)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("sprint show took %s, want well under 10s (<5s)", elapsed)
	}
	if got := probeCalls.Load(); got != 0 {
		t.Fatalf("default sprint show ran %d live resource probes, want 0 (cached-only)", got)
	}
	if !strings.Contains(out, "resources: partial;") {
		t.Fatalf("unexpected resource summary in output:\n%s", out)
	}

	// Explicit --probe-resources opts in to running the live probe.
	out, code = runSprintWith(t, []SprintOption{WithSprintResources(provider)}, "show", "1", "--probe-resources")
	if code != 0 {
		t.Fatalf("sprint show --probe-resources (%d): %s", code, out)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("sprint show --probe-resources probe calls = %d, want 1", got)
	}
}

type slowTestAdapter struct {
	calls *atomic.Int32
}

func (s slowTestAdapter) Kind() string { return "slow" }

func (s slowTestAdapter) Collect(_ context.Context, _ llmbudget.SourceConfig, at time.Time) (llmbudget.SourceResult, error) {
	s.calls.Add(1)
	return llmbudget.SourceResult{Status: "ok"}, nil
}

// Regression for 682f7e66 (2): `sprint goal add --story` and `sprint goal link`
// from an umbrella repo resolve stories stored in submodule docs/todo (bashy,
// yoke, agent-bench) without requiring --repo, and automatically track those
// submodule roots on the sprint card.
func TestSprintGoalAddAndLinkResolveSubmoduleTodoStories(t *testing.T) {
	umbrella := setupIsolationFixture(t)
	seedLiveAgent(t, "mgr-umb")
	t.Setenv("WEAVE_CONDUCTOR", "mgr-umb")

	makeSubmodule := func(name string) string {
		sub := filepath.Join(umbrella, name)
		if err := os.MkdirAll(filepath.Join(sub, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sub, "docs", "todo"), 0o755); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	bashyDir := makeSubmodule("bashy")
	yokeDir := makeSubmodule("yoke")
	benchDir := makeSubmodule("agent-bench")

	gitmodules := strings.Join([]string{
		`[submodule "bashy"]`,
		`	path = bashy`,
		`[submodule "yoke"]`,
		`	path = yoke`,
		`[submodule "agent-bench"]`,
		`	path = agent-bench`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(umbrella, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(umbrella)
	if out, code := runSprint(t, "add", "umbrella sprint"); code != 0 {
		t.Fatalf("sprint add (%d): %s", code, out)
	}

	bashyStory := sprintTestStory(t, bashyDir, 1, "bashy friction story", "p1", todopkg.StatusTodo)
	yokeStory := sprintTestStory(t, yokeDir, 1, "yoke weave story", "p1", todopkg.StatusDone)
	otherSprintStory := sprintTestStory(t, benchDir, 99, "wrong sprint story", "p2", todopkg.StatusTodo)

	// 1. `sprint goal add --story` from umbrella root (no --repo) resolves bashy/docs/todo story.
	out, code := runSprint(t, "goal", "add", "1", "--id", "g-friction", "--text", "fix manager friction", "--story", bashyStory[:8])
	if code != 0 {
		t.Fatalf("sprint goal add --story submodule (%d): %s", code, out)
	}
	if !strings.Contains(out, "linked to story "+bashyStory) {
		t.Fatalf("expected linked confirmation, got:\n%s", out)
	}

	// 2. `sprint goal link --story` from umbrella root (no --repo) resolves yoke/docs/todo story.
	out, code = runSprint(t, "goal", "link", "1", "g-friction", "--story", yokeStory[:8])
	if code != 0 {
		t.Fatalf("sprint goal link --story submodule (%d): %s", code, out)
	}

	// 3. `sprint focus` from umbrella root resolves submodule story too.
	out, code = runSprint(t, "focus", "1", bashyStory[:8])
	if code != 0 {
		t.Fatalf("sprint focus submodule (%d): %s", code, out)
	}

	// 4. Wrong-sprint story in agent-bench/docs/todo reports sprint mismatch, not missing register.
	out, code = runSprint(t, "goal", "add", "1", "--id", "g-wrong", "--text", "wrong", "--story", otherSprintStory[:8])
	if code == 0 || !strings.Contains(out, "belongs to sprint #99, not #1") {
		t.Fatalf("wrong-sprint story = (%d) %s", code, out)
	}

	// Verify both submodule roots were recorded on the sprint card.
	dir, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || len(s.Goal) != 1 || len(s.Goal[0].Stories) != 2 {
		t.Fatalf("sprint goal state = %+v", s)
	}
}

// Regression for 682f7e66 (3): conductor creating, editing, or committing
// story-store files (`docs/todo/<id>-*.md` and the store index) in the live
// checkout while weave runs hold workspaces must NOT trip `isolation_violated`
// or block `weave pull`, MUST surface them as `ignored: story store` in the
// isolation report, and MUST still flag non-store files even under `docs/todo/`.
func TestWeaveIsolationIgnoresConductorDocsTodoEdits(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	for _, okPath := range []string{
		"docs/todo/682f7e66f637-manager-friction.md",
		"docs/todo/1ee0c4e5-short-prefix.md",
		"docs/todo/index.json",
		"docs/todo/.index.json",
		"docs/todo/index.md",
		"docs/todo/.seq",
	} {
		if !weaveIsStoryStorePath(okPath) {
			t.Fatalf("weaveIsStoryStorePath(%q) = false, want true", okPath)
		}
	}
	for _, badPath := range []string{
		"docs/todo/README.md",
		"docs/todo/old-story.md",
		"docs/todo/sneaky.sh",
		"docs/todo/nested/682f7e66f637-card.md",
		"docs/plan-s576.md",
	} {
		if weaveIsStoryStorePath(badPath) {
			t.Fatalf("weaveIsStoryStorePath(%q) = true, want false (not a story-store record or index)", badPath)
		}
	}

	preFixItem := &weaveItem{
		ID:            69,
		LiveRoot:      root,
		LiveTreeSHA:   "legacy-sha",
		LiveTreeLines: []string{" M docs/todo/1ee0c4e59988-prior-story.md"},
	}
	todoOnlyNow := weaveLiveSnapshot{
		SHA:   "different-sha",
		Lines: []string{"?? docs/todo/682f7e66f637-new-story.md", " M docs/todo/index.json"},
	}
	violated, escaped, ignored := weaveIsolationCheck(preFixItem, todoOnlyNow)
	if violated || len(escaped) != 0 || len(ignored) != 3 {
		t.Fatalf("story-store-only diff: violated=%v escaped=%v ignored=%v", violated, escaped, ignored)
	}

	// Non-story file inside docs/todo/ MUST still trip isolation, while keeping story files in ignored.
	sneakyNow := weaveLiveSnapshot{
		SHA:   "sneaky-sha",
		Lines: []string{"?? docs/todo/682f7e66f637-new-story.md", "?? docs/todo/sneaky.sh"},
	}
	violated, escaped, ignored = weaveIsolationCheck(preFixItem, sneakyNow)
	if !violated || len(escaped) != 1 || escaped[0] != "docs/todo/sneaky.sh" || len(ignored) != 2 {
		t.Fatalf("sneaky docs/todo file must violate: violated=%v escaped=%v ignored=%v", violated, escaped, ignored)
	}

	// End-to-end CLI: start a weave run while story-store files are created in
	// the live checkout; status (text + json) lists `ignored: story store`,
	// `isolation_violated` stays false, and `weave pull` merges without --force.
	if _, code := runWeave(t, "add", "worker task while conductor files todos", "--json"); code != 0 {
		t.Fatalf("weave add failed (%d)", code)
	}
	todoDir := filepath.Join(root, "docs", "todo")
	if err := os.MkdirAll(todoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cardPath := filepath.Join(todoDir, "682f7e66f637-manager-friction.md")
	indexPath := filepath.Join(todoDir, "index.json")
	script := fmt.Sprintf(`set -e
echo worker > worker.txt
git add worker.txt
git -c user.email=w@example.invalid -c user.name=worker commit -qm "worker change"
printf -- "---\nid: 682f7e66f637\ntitle: test\n---\n" > %q
printf -- "{}\n" > %q
`, cardPath, indexPath)

	out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("weave start failed (%d):\n%s", code, out)
	}

	statusJSON, code := runWeave(t, "status", "1", "--json")
	if code != 0 {
		t.Fatalf("weave status --json failed (%d):\n%s", code, statusJSON)
	}
	if strings.Contains(statusJSON, `"isolation_violated": true`) {
		t.Fatalf("conductor story-store edit falsely tripped status isolation:\n%s", statusJSON)
	}
	if !strings.Contains(statusJSON, "ignored: story store") || !strings.Contains(statusJSON, "docs/todo/682f7e66f637-manager-friction.md") {
		t.Fatalf("status --json missing 'ignored: story store' listing:\n%s", statusJSON)
	}

	statusText, code := runWeave(t, "status", "1")
	if code != 0 {
		t.Fatalf("weave status text failed (%d):\n%s", code, statusText)
	}
	if !strings.Contains(statusText, "ignored: story store") || !strings.Contains(statusText, "docs/todo/682f7e66f637-manager-friction.md") {
		t.Fatalf("status text missing 'ignored: story store' listing:\n%s", statusText)
	}

	pullOut, code := runWeave(t, "pull", "1", "--json")
	if code != 0 {
		t.Fatalf("weave pull refused over story-store edit (%d):\n%s", code, pullOut)
	}
	if !strings.Contains(pullOut, `"status": "merged"`) {
		t.Fatalf("expected clean merge without --force:\n%s", pullOut)
	}
}
