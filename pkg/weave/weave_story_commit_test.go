package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommitTrace(t *testing.T) {
	message := `feat(board): make progress explicit

The subject and body stay useful to humans.

Sprint: #87
Story: #110
Story-ID: d1e86f29d7a7
Story: #111
Story-ID: a01706e3260f
Signed-off-by: Example <example@example.test>
# Please enter the commit message for your changes. Lines starting with '#'
# are removed by Git after this hook runs.
`
	trace, err := parseCommitTrace(message)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Sprint != 87 || len(trace.Stories) != 2 {
		t.Fatalf("trace = %+v", trace)
	}
	if trace.Stories[0].Number != 110 || trace.Stories[0].ID != "d1e86f29d7a7" {
		t.Fatalf("first story = %+v", trace.Stories[0])
	}
}

func TestParseCommitTraceRefusals(t *testing.T) {
	tests := []struct {
		name, message, want string
	}{
		{"no trailers", "fix: untracked", "subject, a blank line"},
		{"duplicate sprint", "fix: x\n\nSprint: #87\nSprint: #88\nStory: #110\nStory-ID: d1e86f29d7a7", "exactly one"},
		{"missing stable id", "fix: x\n\nSprint: #87\nStory: #110", "matching Story-ID"},
		{"short stable id", "fix: x\n\nSprint: #87\nStory: #110\nStory-ID: d1e86f29", "full 12-character"},
		{"uppercase stable id", "fix: x\n\nSprint: #87\nStory: #110\nStory-ID: D1E86F29D7A7", "lowercase hex"},
		{"bad story marker", "fix: x\n\nSprint: #87\nStory: 110\nStory-ID: d1e86f29d7a7", "Story: #110"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCommitTrace(tt.message)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateCommitTraceStories(t *testing.T) {
	stories := []sprintStoryState{
		{Seq: 110, Ref: sprintStoryRef{ID: "d1e86f29d7a7"}},
		{Seq: 111, Ref: sprintStoryRef{ID: "a01706e3260f"}},
	}
	good := commitTrace{Sprint: 87, Stories: []commitStoryRef{{Number: 110, ID: "d1e86f29d7a7"}}}
	if err := validateCommitTraceStories(good, stories); err != nil {
		t.Fatal(err)
	}
	bad := commitTrace{Sprint: 87, Stories: []commitStoryRef{{Number: 110, ID: "a01706e3260f"}}}
	if err := validateCommitTraceStories(bad, stories); err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("mismatched pair err = %v", err)
	}
	missing := commitTrace{Sprint: 97, Stories: []commitStoryRef{{Number: 16, ID: "0123456789ab"}}}
	if err := validateCommitTraceStories(missing, nil); err == nil ||
		!strings.Contains(err.Error(), "todo add --sprint 97") ||
		!strings.Contains(err.Error(), "weave run is execution") {
		t.Fatalf("missing-story remedy err = %v", err)
	}
}

func TestManagedCommitHookChainsAndValidates(t *testing.T) {
	for _, want := range []string{"commit-msg.before-bashy", `bashy sprint commit-msg "$1"`} {
		if !strings.Contains(managedCommitHook, want) {
			t.Errorf("managed hook missing %q", want)
		}
	}
}

func TestSprintHelpStatesTrackingAndOwnershipContract(t *testing.T) {
	help := NewSprintCmd().Long
	for _, want := range []string{
		"create and link a sprint story before implementation begins",
		"Sprint: #87",
		"bashy sprint hooks install",
		"receives the inbox automatically under the",
		"recorded owner name",
		"Never delete or destroy work owned by another agent",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("sprint help missing %q", want)
		}
	}
}

func TestSprintCommitMsgPrintsActionableRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte("fix: untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"commit-msg", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("invalid commit message passed")
	}
	for _, want := range []string{"commit provenance", "Sprint: #87", "Story-ID: d1e86f29d7a7"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal missing %q:\n%s", want, out.String())
		}
	}
}

func TestInstallSprintHooksWritesPrePushHook(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")

	if _, err := installSprintCommitHook(repo); err != nil {
		t.Fatal(err)
	}
	managed := git("config", "--local", "--get", "core.hooksPath")
	for _, name := range []string{"commit-msg", "pre-push"} {
		info, err := os.Stat(filepath.Join(managed, name))
		if err != nil || info.Mode()&0o111 == 0 {
			t.Fatalf("managed %s hook = info:%v err:%v", name, info, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(managed, "pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		"pre-push.before-bashy",
		"bashy sprint commit-msg --check-range",
		"command -v bashy",
		"merge-base",
		"max-parents=0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("managed pre-push hook missing %q:\n%s", want, body)
		}
	}
}

func TestInstallSprintHooksIdempotentAndPreservesForeignHooks(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")
	hooks := filepath.Join(repo, "project-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignCommit := "#!/bin/sh\nexit 0\n"
	foreignPush := "#!/bin/sh\ncat >/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(foreignCommit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(foreignPush), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "--local", "core.hooksPath", hooks)

	install := func() string {
		t.Helper()
		if _, err := installSprintCommitHook(repo); err != nil {
			t.Fatal(err)
		}
		return git("config", "--local", "--get", "core.hooksPath")
	}
	managed := install()
	for _, tc := range []struct{ name, prior, want string }{
		{"commit-msg", "commit-msg.before-bashy", foreignCommit},
		{"pre-push", "pre-push.before-bashy", foreignPush},
	} {
		prior, err := os.ReadFile(filepath.Join(managed, tc.prior))
		if err != nil || string(prior) != tc.want {
			t.Fatalf("%s preserved = %q, err = %v, want %q", tc.prior, prior, err, tc.want)
		}
		managedBody, err := os.ReadFile(filepath.Join(managed, tc.name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(managedBody), tc.prior) {
			t.Fatalf("managed %s does not chain %s:\n%s", tc.name, tc.prior, managedBody)
		}
	}

	// A second install must not clobber the preserved predecessors.
	again := install()
	if again != managed {
		t.Fatalf("re-install moved hooks path: %q -> %q", managed, again)
	}
	for _, tc := range []struct{ prior, want string }{
		{"commit-msg.before-bashy", foreignCommit},
		{"pre-push.before-bashy", foreignPush},
	} {
		prior, err := os.ReadFile(filepath.Join(managed, tc.prior))
		if err != nil || string(prior) != tc.want {
			t.Fatalf("after re-install %s = %q, err = %v, want %q", tc.prior, prior, err, tc.want)
		}
	}
	prePush, err := os.ReadFile(filepath.Join(managed, "pre-push"))
	if err != nil || string(prePush) != managedPrePushHook {
		t.Fatalf("re-install rewrote pre-push unexpectedly: err=%v:\n%s", err, prePush)
	}
}

func TestManagedPrePushHookChecksPushedRanges(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.test",
		)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "file")
	git("commit", "-qm", "root")
	git("commit", "-qm", "second", "--allow-empty")
	base := git("rev-parse", "HEAD")
	git("checkout", "-qb", "feature")
	git("commit", "-qm", "feature work", "--allow-empty")
	tip := git("rev-parse", "HEAD")
	zero := strings.Repeat("0", 40)

	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bashy-args.log")
	fakeBashy := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_BASHY_LOG\"\nexit \"${FAKE_BASHY_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bashy"), []byte(fakeBashy), 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(t.TempDir(), "pre-push")
	if err := os.WriteFile(hookPath, []byte(managedPrePushHook), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(stdin, exit string) (int, string) {
		t.Helper()
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", hookPath, "origin", "file://"+repo)
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(stdin)
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		cmd.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"FAKE_BASHY_LOG="+logPath,
			"FAKE_BASHY_EXIT="+exit,
		)
		err := cmd.Run()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("run hook: %v\n%s", err, combined.String())
			}
		}
		raw, readErr := os.ReadFile(logPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return code, strings.TrimSpace(string(raw))
	}

	line := "refs/heads/feature " + tip + " refs/heads/feature " + base + "\n"
	if code, logged := run(line, "0"); code != 0 {
		t.Fatalf("passing check exited %d", code)
	} else if want := "sprint commit-msg --check-range " + base + ".." + tip; logged != want {
		t.Fatalf("logged = %q, want %q", logged, want)
	}

	if code, logged := run(line, "1"); code == 0 {
		t.Fatal("a failing check-range must block the push")
	} else if want := "sprint commit-msg --check-range " + base + ".." + tip; logged != want {
		t.Fatalf("logged = %q, want %q", logged, want)
	}

	deletion := "refs/heads/feature " + zero + " refs/heads/feature " + tip + "\n"
	if code, logged := run(deletion, "0"); code != 0 || logged != "" {
		t.Fatalf("deletion: code=%d logged=%q, want no check", code, logged)
	}

	newBranch := "refs/heads/feature " + tip + " refs/heads/feature " + zero + "\n"
	if code, logged := run(newBranch, "0"); code != 0 {
		t.Fatalf("new branch exited %d", code)
	} else if want := "sprint commit-msg --check-range " + base + ".." + tip; logged != want {
		t.Fatalf("new branch logged = %q, want %q", logged, want)
	}
}

func TestManagedPrePushHookChainsForeignHook(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
	}
	git("init", "-q")
	hooks := filepath.Join(repo, "project-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "foreign-ran")
	foreign := "#!/bin/sh\necho ran >> \"" + marker + "\"\ncat >/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "--local", "core.hooksPath", hooks)
	if _, err := installSprintCommitHook(repo); err != nil {
		t.Fatal(err)
	}
	managed, err := func() (string, error) {
		cmd := exec.Command("git", "-C", repo, "config", "--local", "--get", "core.hooksPath")
		raw, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(raw)), err
	}()
	if err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bashy-args.log")
	fakeBashy := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_BASHY_LOG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bashy"), []byte(fakeBashy), 0o755); err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	cmd := exec.Command("/bin/sh", filepath.Join(managed, "pre-push"), "origin", "file://"+repo)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader("refs/heads/x abcdef1234567890abcdef1234567890abcdef12 refs/heads/x " + zero + "\n")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_BASHY_LOG="+logPath,
	)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	// The pushed sha is unknown here; the hook must still chain the foreign
	// hook first and then attempt its own check without hanging on stdin.
	_ = cmd.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("foreign pre-push hook was not chained: %v\n%s", err, combined.String())
	}
}

func TestInstallSprintCommitHookPreservesExistingHooks(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-q")
	hooks := filepath.Join(repo, "project-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	prePush := filepath.Join(hooks, "pre-push")
	if err := os.WriteFile(prePush, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "--local", "core.hooksPath", hooks)

	root, err := installSprintCommitHook(repo)
	if err != nil {
		t.Fatal(err)
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	if root != realRepo {
		t.Fatalf("root = %q, want %q", root, realRepo)
	}
	managed := git("config", "--local", "--get", "core.hooksPath")
	if managed == hooks || !strings.HasSuffix(managed, "bashy-hooks") {
		t.Fatalf("managed hooks path = %q", managed)
	}
	for _, name := range []string{"pre-push", "commit-msg"} {
		info, err := os.Stat(filepath.Join(managed, name))
		if err != nil || info.Mode()&0o111 == 0 {
			t.Fatalf("preserved %s hook = info:%v err:%v", name, info, err)
		}
	}
}

// Sprint 217: the sprint board is per host, but the committed story carries
// its sprint number, so a commit on ANOTHER host validates from git alone —
// no board entry, no session.
func TestSprintCommitMsgAcceptsASprintKnownOnlyFromTheRepoStories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint")) // empty board: sprint #217 is not here
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if raw, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
	}
	git("init", "-q")
	// The story as the manager's host committed it: frontmatter names the sprint.
	story := "---\nid: 0329dd5757b0\nseq: 551\ntitle: Local delivery\nstatus: todo\nsprint: 217\n---\n\nbody\n"
	if err := os.MkdirAll(filepath.Join(repo, "docs", "todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "todo", "0329dd5757b0-local-delivery.md"), []byte(story), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msg, []byte("inbox: deliver\n\nSprint: #217\nStory: #551\nStory-ID: 0329dd5757b0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(path string) (string, error) {
		cmd := NewSprintCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"commit-msg", path})
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := run(msg)
	if err != nil {
		t.Fatalf("a sprint known from the repo's committed story must validate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Sprint #217, 1 story reference(s) verified") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	// A sprint nobody's story names is still refused, with the cross-host hint.
	bad := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(bad, []byte("x\n\nSprint: #999\nStory: #551\nStory-ID: 0329dd5757b0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run(bad)
	if err == nil || !strings.Contains(out, "not on this host's sprint board and no committed story") {
		t.Fatalf("want the cross-host refusal, got err=%v\n%s", err, out)
	}
}

// Sprint 246: `git clone` never copies hooks, so a weave workspace started
// without the sprint provenance hook even when its source repo was fail-closed.
// The agent then committed a malformed `Story:` trailer with no feedback and
// the defect surfaced only at `weave pull` — too late to fix cheaply, because
// the agent's commits were already written. weaveSourceEnforcesCommitHook is
// what lets workspace creation mirror the source's enforcement.
func TestWeaveSourceEnforcesCommitHookDetectsAnInstalledHook(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
	}
	git("init", "-q")

	if weaveSourceEnforcesCommitHook(repo) {
		t.Fatal("a fresh repo enforces nothing, but the helper said it does")
	}
	if _, err := installSprintCommitHook(repo); err != nil {
		t.Fatal(err)
	}
	if !weaveSourceEnforcesCommitHook(repo) {
		t.Fatal("hook installed, but the helper did not see it")
	}

	// A clone does not inherit it — the bug this guards.
	clone := filepath.Join(t.TempDir(), "workspace")
	if raw, err := exec.Command("git", "clone", "--local", "--no-hardlinks", "-q", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, raw)
	}
	if weaveSourceEnforcesCommitHook(clone) {
		t.Fatal("the clone reported enforcement it never received; the premise of the fix is wrong")
	}
	// Installing it into the clone is what workspace creation now does.
	if _, err := installSprintCommitHook(clone); err != nil {
		t.Fatal(err)
	}
	if !weaveSourceEnforcesCommitHook(clone) {
		t.Fatal("hook installed into the clone, but the helper did not see it")
	}
}

func TestParseCommitAgentTrailer(t *testing.T) {
	base := "deliver\n\nSprint: #87\nStory: #110\nStory-ID: d1e86f29d7a7\n"
	for _, agent := range []string{"agent-a", "tool-a:model-a", ""} {
		trace, err := parseCommitTrace(base + "Agent: " + agent + "\n")
		if err != nil || !trace.AgentPresent || trace.Agent != agent {
			t.Fatalf("trace=%+v err=%v", trace, err)
		}
	}
	if _, err := parseCommitTrace(base + "Agent: agent-a\nAgent: agent-b\n"); err == nil {
		t.Fatal("duplicate Agent accepted")
	}
}
