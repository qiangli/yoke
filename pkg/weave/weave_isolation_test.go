package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupIsolationFixture builds a live repo with a seed commit and points
// HOME at a temp dir, so the queue lands under an isolated weave state
// root. Returns the repo root as GIT resolves it — on macOS t.TempDir()
// hands back a /var symlink whose real path is /private/var, and the item's
// recorded LiveRoot is the resolved one.
func setupIsolationFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	// The isolated HOME has no ~/.gitconfig, so the seed commit here and weave's
	// own internal merge commits would fail with "Committer identity unknown" on a
	// host whose identity lives only in the (now-hidden) global config. Give the
	// isolated HOME its own identity so the fixture is HERMETIC — it must not
	// depend on the developer's machine having a global git user configured.
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Weave Test\n\temail = weave-test@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "seed.txt")
	gitT(t, dir, "commit", "-qm", "seed")
	root, err := weaveRepoRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestWeaveSnapshotLiveTreeDetectsChange pins the primitive: the
// fingerprint is stable across repeated reads of an unchanged tree (or the
// guard would flag every run), and moves when a file appears.
func TestWeaveSnapshotLiveTreeDetectsChange(t *testing.T) {
	root := setupIsolationFixture(t)

	first, err := weaveSnapshotLiveTree(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if first.SHA == "" {
		t.Fatal("clean tree must still produce a fingerprint (empty status hashes to something)")
	}
	again, err := weaveSnapshotLiveTree(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if again.SHA != first.SHA {
		t.Fatalf("fingerprint of an unchanged tree must be stable; %s != %s", again.SHA, first.SHA)
	}

	if err := os.WriteFile(filepath.Join(root, "escape.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := weaveSnapshotLiveTree(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if after.SHA == first.SHA {
		t.Fatal("an untracked file appearing in the live tree must move the fingerprint")
	}
}

// TestWeaveIsolationStatus covers the comparison itself: clean-vs-clean is
// silent, a new file is named, a DELETED baseline entry is also named (an
// agent that removed the human's work escaped just as much as one that
// added a file), and a run with no baseline is never flagged.
func TestWeaveIsolationStatus(t *testing.T) {
	base := weaveLiveSnapshot{SHA: "aaa", Lines: []string{" M keep.go", "?? human-wip.txt"}}

	it := &weaveItem{ID: 1, LiveRoot: "/live", LiveTreeSHA: base.SHA, LiveTreeLines: base.Lines}

	if violated, _ := weaveIsolationStatus(it, base); violated {
		t.Fatal("identical fingerprint must not read as a violation")
	}

	// The agent created a patch script in the live checkout — the real
	// incident. Only the new path is named; the human's own pre-existing
	// dirt was in the baseline and is not the run's doing.
	now := weaveLiveSnapshot{SHA: "bbb", Lines: []string{" M keep.go", "?? human-wip.txt", "?? apply-patch.sh"}}
	violated, escaped := weaveIsolationStatus(it, now)
	if !violated {
		t.Fatal("a file appearing in the live tree during the run must flag the run")
	}
	if len(escaped) != 1 || escaped[0] != "apply-patch.sh" {
		t.Fatalf("escaped paths should name only the new file, got %v", escaped)
	}

	// The agent reverted the human's in-flight edit: the baseline line is
	// gone. Destruction is an escape too.
	gone := weaveLiveSnapshot{SHA: "ccc", Lines: []string{" M keep.go"}}
	violated, escaped = weaveIsolationStatus(it, gone)
	if !violated {
		t.Fatal("a baseline entry disappearing must flag the run")
	}
	if len(escaped) != 1 || escaped[0] != "human-wip.txt" {
		t.Fatalf("escaped paths should name the vanished file, got %v", escaped)
	}

	// No baseline (pre-guard item, or a root we couldn't stat): silent.
	// A guard that guesses would flag every legacy item in the queue.
	if violated, _ := weaveIsolationStatus(&weaveItem{ID: 2}, now); violated {
		t.Fatal("an item with no baseline must never be flagged")
	}

	// Truncated baseline: change is still DETECTED via the SHA, with no
	// paths to name.
	big := &weaveItem{ID: 3, LiveTreeSHA: "aaa", LiveTreeTruncated: true}
	violated, escaped = weaveIsolationStatus(big, now)
	if !violated {
		t.Fatal("a truncated baseline must still detect change via the SHA")
	}
	if len(escaped) != 0 {
		t.Fatalf("a truncated baseline cannot name paths, got %v", escaped)
	}
}

// TestWeavePorcelainPath pins path extraction, including the rename form
// (the arrow) that a naive line[3:] would report as "old -> new".
func TestWeavePorcelainPath(t *testing.T) {
	cases := map[string]string{
		"?? apply-patch.sh":     "apply-patch.sh",
		" M pkg/weave/weave.go": "pkg/weave/weave.go",
		"A  new.txt":            "new.txt",
		"R  old.txt -> new.txt": "new.txt",
		"?? dir/with space.txt": "dir/with space.txt",
	}
	for line, want := range cases {
		if got := weavePorcelainPath(line); got != want {
			t.Errorf("weavePorcelainPath(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestWeaveApplyIsolationCheckSticky: a run that escaped and then tidied
// up after itself still escaped. If the flag evaporated whenever the tree
// happened to match again, the guard would be defeated by an agent that
// cleaned up — or simply by the human reverting the damage before looking.
func TestWeaveApplyIsolationCheckSticky(t *testing.T) {
	root := setupIsolationFixture(t)

	it := &weaveItem{ID: 1}
	weaveRecordLiveBaseline(it, root)
	if it.LiveTreeSHA == "" {
		t.Fatal("baseline should have been recorded for a readable repo")
	}

	escape := filepath.Join(root, "apply-patch.sh")
	if err := os.WriteFile(escape, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if newly := weaveApplyIsolationCheck(it); !newly {
		t.Fatal("first check after an escape should report the flag as newly set")
	}
	if !it.IsolationViolated {
		t.Fatal("the escape must be recorded on the item")
	}
	if len(it.EscapedPaths) != 1 || it.EscapedPaths[0] != "apply-patch.sh" {
		t.Fatalf("escaped paths should name the file, got %v", it.EscapedPaths)
	}

	// Tidy up: the tree now matches the baseline again.
	if err := os.Remove(escape); err != nil {
		t.Fatal(err)
	}
	if newly := weaveApplyIsolationCheck(it); newly {
		t.Fatal("a second check should not re-report an already-known violation")
	}
	if !it.IsolationViolated {
		t.Fatal("the flag must be STICKY — a run that cleaned up after itself still escaped")
	}
	if len(it.EscapedPaths) != 1 {
		t.Fatalf("the escaped-path record must survive the cleanup, got %v", it.EscapedPaths)
	}
}

// TestWeaveIsolationEndToEnd is the whole point, driven through the real
// CLI: a subagent that reaches OUT of its workspace and writes into the
// live checkout is flagged, says so in status/list, and — the load-bearing
// half — is REFUSED by `weave pull` until --force.
func TestWeaveIsolationEndToEnd(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if _, code := runWeave(t, "add", "escape the workspace", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d)", code)
	}

	// The subagent: does honest work in its workspace (a real commit, so
	// the branch is genuinely mergeable and only the isolation gate can
	// stop it), then escapes to the live checkout and writes a patch
	// script there — exactly the observed incident.
	script := `set -e
	echo fix > fix.txt
	git add fix.txt
	git -c user.email=a@a -c user.name=a commit -qm "the real work"
	printf '#!/bin/sh\n# oops\n' > ` + filepath.Join(root, "apply-patch.sh")

	out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("weave start failed (exit %d):\n%s", code, out)
	}

	// The escape landed in the live checkout — the bug this guard exists
	// to catch. (Assert the premise, so a fixture that silently stopped
	// escaping can't make the rest of the test vacuously pass.)
	if _, err := os.Stat(filepath.Join(root, "apply-patch.sh")); err != nil {
		t.Fatalf("fixture did not actually escape into the live checkout: %v", err)
	}

	statusOut, code := runWeave(t, "status", "1", "--json")
	if code != 0 {
		t.Fatalf("weave status failed (exit %d):\n%s", code, statusOut)
	}
	if !strings.Contains(statusOut, `"isolation_violated": true`) {
		t.Fatalf("status must flag the escaped run:\n%s", statusOut)
	}
	if !strings.Contains(statusOut, "apply-patch.sh") {
		t.Fatalf("status must NAME the escaped path — a flag with no path is a dead end:\n%s", statusOut)
	}

	listOut, code := runWeave(t, "list", "--json")
	if code != 0 {
		t.Fatalf("weave list failed (exit %d):\n%s", code, listOut)
	}
	if !strings.Contains(listOut, `"isolation_violated": true`) {
		t.Fatalf("list must surface the flag:\n%s", listOut)
	}

	// THE GATE. The branch has a real commit and would otherwise merge.
	pullOut, _ := runWeave(t, "pull", "1", "--json")
	if !strings.Contains(pullOut, "isolation-violated") {
		t.Fatalf("pull must refuse the escaped run:\n%s", pullOut)
	}
	if strings.Contains(pullOut, `"status": "merged"`) {
		t.Fatalf("pull merged an isolation-violated run — the whole bug:\n%s", pullOut)
	}
	// Refused means REFUSED: the work must not have landed in main.
	if log := gitT(t, root, "log", "--oneline", "main"); strings.Contains(log, "the real work") {
		t.Fatalf("the escaped run's commit reached main despite the refusal:\n%s", log)
	}

	// --force is the deliberate override: the operator reviewed the paths
	// and decided (commonly: "that edit was mine").
	forceOut, code := runWeave(t, "pull", "1", "--force", "--json")
	if code != 0 {
		t.Fatalf("weave pull --force failed (exit %d):\n%s", code, forceOut)
	}
	if !strings.Contains(forceOut, `"status": "merged"`) {
		t.Fatalf("--force must merge the flagged run:\n%s", forceOut)
	}
	if log := gitT(t, root, "log", "--oneline", "main"); !strings.Contains(log, "the real work") {
		t.Fatalf("--force merged but the commit is not in main:\n%s", log)
	}
}

// TestWeaveIsolationResumeRebaselines: the verdict describes ONE run. A
// resumed run is a new run, judged against the live checkout as it stands
// at resume — so an escape left un-cleaned does not wedge the issue,
// refusing every retry for damage the new run did not do. (The escape was
// already surfaced and blocked at the time it happened; resume is a
// deliberate operator act.)
func TestWeaveIsolationResumeRebaselines(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if _, code := runWeave(t, "add", "escape then retry", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d)", code)
	}
	escaping := `printf 'oops\n' > ` + filepath.Join(root, "apply-patch.sh")
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", escaping); code != 0 {
		t.Fatalf("weave start failed (exit %d):\n%s", code, out)
	}
	statusOut, _ := runWeave(t, "status", "1", "--json")
	if !strings.Contains(statusOut, `"isolation_violated": true`) {
		t.Fatalf("the escaping run must be flagged:\n%s", statusOut)
	}

	// Retry in the same workspace. The escape file is STILL in the live
	// tree (nobody cleaned it) — it belongs to the new run's baseline now.
	behaving := `set -e
	echo fix > fix.txt
	git add fix.txt
	git -c user.email=a@a -c user.name=a commit -qm "retry work"`
	out, code := runWeave(t, "start", "--issue", "1", "--resume", "--json", "--", "sh", "-c", behaving)
	if code != 0 {
		t.Fatalf("weave start --resume failed (exit %d):\n%s", code, out)
	}
	statusOut, _ = runWeave(t, "status", "1", "--json")
	if strings.Contains(statusOut, `"isolation_violated": true`) {
		t.Fatalf("the retry stayed in its workspace and must not inherit the prior run's verdict:\n%s", statusOut)
	}
	pullOut, code := runWeave(t, "pull", "1", "--json")
	if code != 0 {
		t.Fatalf("weave pull failed (exit %d):\n%s", code, pullOut)
	}
	if !strings.Contains(pullOut, `"status": "merged"`) {
		t.Fatalf("a clean retry must merge without --force:\n%s", pullOut)
	}
}

// TestWeaveIsolationCleanRunNotFlagged is the false-positive guard. A run
// that stays inside its workspace must merge with no ceremony — a guard
// that flags honest runs gets --force'd reflexively, and then it guards
// nothing.
func TestWeaveIsolationCleanRunNotFlagged(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if _, code := runWeave(t, "add", "behave", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d)", code)
	}
	script := `set -e
	echo fix > fix.txt
	git add fix.txt
	git -c user.email=a@a -c user.name=a commit -qm "well-behaved work"`
	out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("weave start failed (exit %d):\n%s", code, out)
	}
	if strings.Contains(out, "isolation-violated") || strings.Contains(out, "ISOLATION") {
		t.Fatalf("a well-behaved run must not be warned about:\n%s", out)
	}

	statusOut, _ := runWeave(t, "status", "1", "--json")
	if strings.Contains(statusOut, `"isolation_violated": true`) {
		t.Fatalf("a well-behaved run must not be flagged:\n%s", statusOut)
	}

	pullOut, code := runWeave(t, "pull", "1", "--json")
	if code != 0 {
		t.Fatalf("weave pull failed (exit %d):\n%s", code, pullOut)
	}
	if !strings.Contains(pullOut, `"status": "merged"`) {
		t.Fatalf("a well-behaved run must merge without --force:\n%s", pullOut)
	}
}
