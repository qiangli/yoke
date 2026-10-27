package loom

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The bug this story fixes, expressed as a test: a Gitea-shaped hook carries
// `#!/usr/bin/env bash`, and the managed git tree ships bash only under the
// name "sh". Before ensureHookShell the tree cannot serve the interpreter the
// hook asks for; after it, the hook — and the nested post-receive.d script
// Gitea delegates to, which carries the same shebang — run through the bash
// IN THAT TREE.
//
// Windows resolves the shebang inside git/msys rather than the kernel, so this
// runs the same mechanism in the shape the host OS can: a tree whose "sh" is
// the platform bash, a PATH that starts with that tree, and $BASH as the
// witness of which interpreter actually ran.
func TestHookShellRunsGiteaStyleHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		// On Windows the shebang is resolved by git and msys, not by the
		// kernel — and not by Go's os/exec either, which refuses an
		// extensionless script before CreateProcess can even report it. The
		// chain can only be driven end to end there by a real push through the
		// provisioned git, which is the live half of this story
		// (docs/s515-4-live-round3.md). What is checkable in a unit test on
		// Windows — ensureHookShell, the tree layout, the PATH — is covered by
		// the tests below, and those do run there.
		t.Skip("hook shebang resolution belongs to git/msys on Windows; covered live")
	}
	dir := t.TempDir()
	usrBin := filepath.Join(dir, "usr", "bin")
	stageShell(t, usrBin)
	bash := filepath.Join(usrBin, hookShellName("bash"))

	// A post-receive pair shaped like the one Gitea generates: the delegating
	// hook it writes per repo, and the post-receive.d script that calls back
	// into the forge (here a marker standing in for `gitea hook post-receive`).
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(filepath.Join(hooks, "post-receive.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "post-receive")
	writeExe(t, hook, `#!/usr/bin/env bash
data=$(cat)
exitcodes=""
hookname=$(basename $0)
GIT_DIR=${GIT_DIR:-$(dirname $0)/..}

for h in ${GIT_DIR}/hooks/${hookname}.d/*; do
test -x "${h}" && test -f "${h}" || continue
echo "${data}" | "${h}"
exitcodes="${exitcodes} $?"
done

for i in ${exitcodes}; do
[ ${i} -eq 0 ] || exit ${i}
done
`)
	writeExe(t, filepath.Join(hooks, "post-receive.d", "gitea"), `#!/usr/bin/env bash
echo "gitea-hook interpreter=$BASH refs=$(cat)"
`)

	run := func() (string, error) {
		cmd := exec.Command(hook)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("0000 1111 refs/heads/main\n")
		// usrBin first, so `env bash` can only resolve to the tree's own shell.
		cmd.Env = append(os.Environ(), "PATH="+usrBin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// RED: the tree has no "bash", so the hook cannot run through the tree's
	// own shell. Whatever happens, it must not be our bash — there is none.
	if out, err := run(); err == nil && strings.Contains(out, "interpreter="+bash) {
		t.Fatalf("hook already ran through %s before provisioning: %s", bash, out)
	}

	// GREEN: provision the name, and the whole chain runs through that tree.
	got, err := ensureHookShell(usrBin)
	if err != nil {
		t.Fatal(err)
	}
	if got != bash {
		t.Fatalf("hook shell: got %q want %q", got, bash)
	}
	out, err := run()
	if err != nil {
		t.Fatalf("hook failed: %v\n%s", err, out)
	}
	for _, want := range []string{"gitea-hook", "interpreter=" + bash, "refs=0000 1111 refs/heads/main"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in hook output: %s", want, out)
		}
	}
}

func TestEnsureHookShellIdempotentAndLoud(t *testing.T) {
	usrBin := filepath.Join(t.TempDir(), "usr", "bin")
	if err := os.MkdirAll(usrBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureHookShell(usrBin); err == nil {
		t.Fatal("want an error with no shell to link")
	}
	if _, err := ensureHookShell(""); err == nil {
		t.Fatal("want an error with no tree")
	}
	writeExe(t, filepath.Join(usrBin, hookShellName("sh")), "shell-one")
	bash, err := ensureHookShell(usrBin)
	if err != nil {
		t.Fatal(err)
	}
	// A second start must leave the existing name alone, not replace it: on
	// Windows the file may be executing a hook right now.
	writeExe(t, bash, "shell-two")
	again, err := ensureHookShell(usrBin)
	if err != nil || again != bash {
		t.Fatalf("got %q, %v", again, err)
	}
	b, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "shell-two" {
		t.Fatalf("existing hook shell was replaced: %q", b)
	}
}

func TestGitRootFromExe(t *testing.T) {
	// Only the managed <root>/cmd/git.exe layout yields a tree.
	if root := gitRootFromExe(""); root != "" {
		t.Fatalf("no git exe: got %q", root)
	}
	if root := gitRootFromExe(filepath.Join("opt", "bin", "git")); root != "" {
		t.Fatalf("unexpected layout accepted: %q", root)
	}
	root := filepath.Join("cache", "mingit", "2.55.0.2")
	if got := gitRootFromExe(filepath.Join(root, "cmd", "git.exe")); got != root {
		t.Fatalf("root: got %q want %q", got, root)
	}
}

// stageShell puts a working bash in usrBin under the name "sh", the way the
// managed MinGit tree ships it (usr/bin/sh.exe IS the bash package, and there
// is no bash.exe). A candidate is accepted only once a LINKED copy of it runs
// from that directory — the way the hook will reach it — so a launcher that
// needs siblings, or a platform binary this OS refuses to run from outside its
// own tree, is passed over rather than failing the test for the host's reasons.
func stageShell(t *testing.T, usrBin string) {
	t.Helper()
	var candidates []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, hookShellName("bash")))
		}
	}
	candidates = append(candidates, "/bin/bash", "/usr/bin/bash")
	sh := filepath.Join(usrBin, hookShellName("sh"))
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.RemoveAll(usrBin); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(usrBin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := copyShell(candidate, sh); err != nil {
			continue
		}
		probe := filepath.Join(usrBin, hookShellName("probe"))
		if err := os.Link(sh, probe); err != nil {
			continue
		}
		err := exec.Command(probe, "-c", "exit 0").Run()
		if rerr := os.Remove(probe); rerr != nil {
			t.Fatal(rerr)
		}
		if err == nil {
			return
		}
	}
	t.Skip("no bash on this host can be staged into a tree of its own")
}
