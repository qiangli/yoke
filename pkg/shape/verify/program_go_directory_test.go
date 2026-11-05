package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/evidence"
)

func TestProgramGoRelativeDirectoryLayouts(t *testing.T) {
	for _, argv := range [][]string{{"go", "-C", "net/server", "test", "-json", "./..."}, {"go.exe", "-C=net/server", "test", "-json", "./..."}, {"bashy.exe", "go", "-C", "net/server", "test", "-json", "./..."}, {"/trusted/bashy.real", "go", "-C", "net/server", "test", "-json", "./..."}, {"bashy", "go", "-C=net/server", "test", "-json", "./..."}} {
		if _, native, e := nativeGoTest(argv); e != nil || !native {
			t.Fatalf("%q: %v", argv, e)
		}
	}
	for _, dir := range []string{"", "/outside", "../outside", "net/../outside", `C:\outside`, `\\server\share`, `net\..\outside`} {
		if _, _, e := nativeGoTest([]string{"go", "-C", dir, "test", "-json", "./..."}); e == nil {
			t.Fatalf("unsafe -C %q accepted", dir)
		}
	}
	for _, argv := range [][]string{{"go", "-C"}, {"go", "-C=net/server", "-C=other", "test", "-json"}, {"bashy", "go", "-C", "net/server", "-x", "test", "-json"}} {
		if _, _, e := nativeGoTest(argv); e == nil {
			t.Fatalf("unsupported %q accepted", argv)
		}
	}
}
func TestProgramGoDirectoryConfinedInEveryClone(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	for _, root := range roots {
		if e := os.MkdirAll(filepath.Join(root, "net", "server"), 0700); e != nil {
			t.Fatal(e)
		}
		if e := confineProgramGoDirectory(root, "net/server"); e != nil {
			t.Fatal(e)
		}
	}
	if e := confineProgramGoDirectory(roots[0], "../outside"); e == nil {
		t.Fatal("parent escaped")
	}
	if runtime.GOOS == "windows" {
		return
	} // Link creation needs a host privilege unavailable on some Windows runners.
	outside := t.TempDir()
	for _, root := range roots {
		if e := os.RemoveAll(filepath.Join(root, "net", "server")); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, filepath.Join(root, "net", "server")); e != nil {
			t.Fatal(e)
		}
		if e := confineProgramGoDirectory(root, "net/server"); e == nil {
			t.Fatal("symlink escaped exact clone")
		}
	}
}
func TestProgramGoNestedModuleUsesRealBaselineAccounting(t *testing.T) {
	repoDir := t.TempDir()
	repo, e := gogit.PlainInit(repoDir, false)
	if e != nil {
		t.Fatal(e)
	}
	files := map[string]string{"README.md": "nested module fixture", "net/server/go.mod": "module nestedfixture\n\ngo 1.20\n", "net/server/main.go": "package main\nfunc main() {}\n", "net/server/main_test.go": "package main\nimport \"testing\"\nfunc TestBaseline(t *testing.T) {}\n"}
	for name, body := range files {
		p := filepath.Join(repoDir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0700)
		if e = os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	commit := func(message string) string {
		t.Helper()
		wt, _ := repo.Worktree()
		if e = wt.AddWithOptions(&gogit.AddOptions{All: true}); e != nil {
			t.Fatal(e)
		}
		h, e := wt.Commit(message, &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
		if e != nil {
			t.Fatal(e)
		}
		return h.String()
	}
	base := commit("base")
	runner := ProgramRunnerFunc(func(ctx context.Context, r ProgramRun) error {
		cmd := exec.CommandContext(ctx, r.Argv[0], r.Argv[1:]...)
		cmd.Dir = r.Dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
		out, e := cmd.CombinedOutput()
		if r.Output != nil {
			*r.Output = string(out)
		}
		return e
	})
	opts := ProgramOptions{Home: t.TempDir(), App: "nested", Kind: "cli", RepoDir: repoDir, BaseSHA: base, CandidateSHA: base, Principal: "host", Policy: "nested-test-policy", Build: []string{"go", "-C", "net/server", "build", "-o", "{output}", "."}, Test: []string{"go", "-C", "net/server", "test", "-json", "-count=1", "./..."}, Runner: runner}
	report, e := RunProgram(context.Background(), opts)
	if e != nil || report.Record.Verdict != evidence.VerdictPass {
		t.Fatalf("baseline %+v %v", report.Record, e)
	}
	if e = os.WriteFile(filepath.Join(repoDir, "net", "server", "main.go"), []byte("package main\nimport \"os\"\nfunc init(){os.Exit(0)}\nfunc main(){}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	opts.CandidateSHA = commit("candidate early exit")
	report, e = RunProgram(context.Background(), opts)
	if e != nil || report.Record.Verdict != evidence.VerdictFail || !strings.Contains(report.Record.Reason, "did not complete") {
		t.Fatalf("candidate bypass accepted %+v %v", report.Record, e)
	}
}
func TestProgramGoRejectsEscapingCandidateBeforeBaselineExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	} // Symlink creation may require elevated host privileges.
	repoDir := t.TempDir()
	repo, e := gogit.PlainInit(repoDir, false)
	if e != nil {
		t.Fatal(e)
	}
	module := filepath.Join(repoDir, "net", "server")
	os.MkdirAll(module, 0700)
	os.WriteFile(filepath.Join(module, "go.mod"), []byte("module confinedfixture\n"), 0600)
	wt, _ := repo.Worktree()
	wt.AddWithOptions(&gogit.AddOptions{All: true})
	base, e := wt.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
	if e != nil {
		t.Fatal(e)
	}
	isolated := t.TempDir()
	os.MkdirAll(filepath.Join(isolated, "net"), 0700)
	if e = os.Symlink(t.TempDir(), filepath.Join(isolated, "net", "server")); e != nil {
		t.Fatal(e)
	}
	calls := 0
	runner := ProgramRunnerFunc(func(context.Context, ProgramRun) error { calls++; return nil })
	_, _, e = programBaselineChecks(context.Background(), ProgramOptions{RepoDir: repoDir, BaseSHA: base.String(), Test: []string{"go", "-C", "net/server", "test", "-json", "./..."}}, runner, isolated)
	if e == nil || !strings.Contains(e.Error(), "escapes") || calls != 0 {
		t.Fatalf("escaping candidate executed: calls=%d err=%v", calls, e)
	}
}
