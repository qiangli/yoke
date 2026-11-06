package verify

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/evidence"
)

type programRuntimeTrace struct{ Stage, Home, Temp, ModuleCache, BuildCache string }

func appendProgramRuntimeTrace(path, stage string) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(programRuntimeTrace{stage, os.Getenv("HOME"), os.Getenv("TMPDIR"), os.Getenv("GOMODCACHE"), os.Getenv("GOCACHE")})
}
func TestProgramRuntimeBuildChild(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != "program-runtime-build-child" {
		return
	}
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	if programPathInside(cwd, os.Getenv("HOME")) || programPathInside(cwd, os.TempDir()) {
		t.Fatal("build runtime home/temp inside source")
	}
	if os.Getenv("SHAPE_PARENT_RUNTIME_SECRET") != "" {
		t.Fatal("parent secret inherited")
	}
	if e := appendProgramRuntimeTrace(args[1], "build"); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("go", "-C", "net/server", "build", "-o", args[2], ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e := cmd.Run(); e != nil {
		t.Fatal(e)
	}
}
func TestProgramDefaultRunnerOwnsExternalRuntimeAcrossAllStages(t *testing.T) {
	repoDir := t.TempDir()
	repo, e := gogit.PlainInit(repoDir, false)
	if e != nil {
		t.Fatal(e)
	}
	trace := filepath.Join(t.TempDir(), "runtime.jsonl")
	testBody := fmt.Sprintf(`package main
import("encoding/json";"flag";"os";"path/filepath";"testing")
func TestMain(m *testing.M){
 flag.Parse()
 stage:="test";if flag.Lookup("test.list").Value.String()!=""{stage="listing"}
 f,err:=os.OpenFile(%q,os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);if err!=nil{panic(err)}
 json.NewEncoder(f).Encode(map[string]string{"Stage":stage,"Home":os.Getenv("HOME"),"Temp":os.Getenv("TMPDIR"),"ModuleCache":os.Getenv("GOMODCACHE"),"BuildCache":os.Getenv("GOCACHE")});f.Close()
 if os.Getenv("SHAPE_PARENT_RUNTIME_SECRET")!=""{panic("parent secret inherited")}
 scratch,err:=os.MkdirTemp("","fixture-");if err!=nil{panic(err)}
 for _,dir:=range []string{filepath.Join(os.Getenv("GOMODCACHE"),"example.invalid","module@v1"),filepath.Join(os.Getenv("HOME"),"go","pkg","mod","simulated@v1"),scratch}{
  if err=os.MkdirAll(dir,0700);err!=nil{panic(err)}
  if err=os.WriteFile(filepath.Join(dir,"go.mod"),[]byte("module runtime-only\n"),0600);err!=nil{panic(err)}
 }
 os.Exit(m.Run())
}
func TestBaseline(t *testing.T){if err:=os.WriteFile(filepath.Join(t.TempDir(),"go.mod"),[]byte("module temporary-fixture\n"),0600);err!=nil{t.Fatal(err)}}
`, trace)
	for name, body := range map[string]string{"net/server/go.mod": "module runtimefixture\n\ngo 1.20\n", "net/server/main.go": "package main\nfunc main(){}\n", "net/server/main_test.go": testBody} {
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
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	opts := ProgramOptions{Home: t.TempDir(), App: "runtimefixture", Kind: "cli", RepoDir: repoDir, BaseSHA: base, CandidateSHA: base, Principal: "host", Policy: "runtime-policy", Build: []string{exe, "-test.run=^TestProgramRuntimeBuildChild$", "--", "program-runtime-build-child", trace, "{output}"}, Test: []string{"go", "-C", "net/server", "test", "-json", "-count=1", "./..."}}
	t.Setenv("SHAPE_PARENT_RUNTIME_SECRET", "must-not-inherit")
	assertRuntime := func() {
		t.Helper()
		body, e := os.ReadFile(trace)
		if e != nil {
			t.Fatal(e)
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) != 3 {
			t.Fatalf("runtime stages: %s", body)
		}
		var home string
		for i, line := range lines {
			var row programRuntimeTrace
			if e = json.Unmarshal([]byte(line), &row); e != nil {
				t.Fatal(e)
			}
			if row.Stage != []string{"listing", "build", "test"}[i] {
				t.Fatal(row)
			}
			if home == "" {
				home = row.Home
			}
			if row.Home != home {
				t.Fatal("runtime not shared", row)
			}
			for _, path := range []string{row.Home, row.Temp, row.ModuleCache, row.BuildCache} {
				if !filepath.IsAbs(path) || programPathInside(repoDir, path) {
					t.Fatal("runtime path inside source", path)
				}
				if _, e = os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("runtime not cleaned after verification", path, e)
				}
			}
		}
	}
	report, e := RunProgram(context.Background(), opts)
	if e != nil || report.Record.Verdict != evidence.VerdictPass {
		t.Fatalf("runtime cache/temp falsely affected source: %+v %v", report.Record, e)
	}
	assertRuntime()
	if e = os.Remove(trace); e != nil {
		t.Fatal(e)
	}
	code := "package main\nimport(\"os\";\"path/filepath\")\nfunc init(){p:=filepath.Join(\"go\",\"pkg\",\"mod\",\"source-mutation\");if err:=os.MkdirAll(p,0700);err!=nil{panic(err)};if err:=os.WriteFile(filepath.Join(p,\"go.mod\"),[]byte(\"module source-mutation\\n\"),0600);err!=nil{panic(err)}}\nfunc main(){}\n"
	if e = os.WriteFile(filepath.Join(repoDir, "net", "server", "main.go"), []byte(code), 0600); e != nil {
		t.Fatal(e)
	}
	opts.CandidateSHA = commit("true source manifest mutation")
	report, e = RunProgram(context.Background(), opts)
	if e != nil || report.Record.Verdict != evidence.VerdictFail || !strings.Contains(report.Record.Reason, "changed dependency manifests") {
		t.Fatalf("real source manifest mutation accepted: %+v %v", report.Record, e)
	}
	assertRuntime()
}
func TestProgramRuntimeEnvironmentWindowsProfileMapping(t *testing.T) {
	root := t.TempDir()
	env, e := programRuntimeEnvironment(root, "windows")
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"HOME", "TMPDIR", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "GOPATH", "GOMODCACHE", "GOCACHE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "BASHY_HOME"} {
		if !programPathInside(root, env[key]) {
			t.Fatalf("%s escaped owned runtime: %q", key, env[key])
		}
	}
	if env["USERPROFILE"] != env["HOME"] || env["TEMP"] != env["TMPDIR"] || env["TMP"] != env["TMPDIR"] {
		t.Fatal(env)
	}
}
func TestProgramRuntimeRejectsSourceContainedTempBeforeWrites(t *testing.T) {
	source := t.TempDir()
	t.Setenv("TMPDIR", source)
	t.Setenv("TEMP", source)
	t.Setenv("TMP", source)
	if _, e := newProgramRuntimeRoot(source); e == nil || !strings.Contains(e.Error(), "outside source") {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(source)
	if e != nil || len(entries) != 0 {
		t.Fatalf("bad temp root changed source: %v %v", entries, e)
	}
}
func TestProgramRuntimeCallChild(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != "program-runtime-call-child" {
		return
	}
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	if programPathInside(cwd, os.Getenv("HOME")) || programPathInside(cwd, os.TempDir()) {
		t.Fatal("runtime home/temp inside source")
	}
	if e = appendProgramRuntimeTrace(args[1], args[2]); e != nil {
		t.Fatal(e)
	}
	if args[2] == "fail" {
		t.Fatal("intentional contained failure")
	}
}
func TestProgramZeroValueRunnerOwnsAndCleansEachCall(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	source := t.TempDir()
	trace := filepath.Join(t.TempDir(), "calls.jsonl")
	for _, mode := range []string{"pass", "fail"} {
		e = (ContainedProgramRunner{}).RunProgram(context.Background(), ProgramRun{Dir: source, Argv: []string{exe, "-test.run=^TestProgramRuntimeCallChild$", "--", "program-runtime-call-child", trace, mode}, Timeout: 10 * time.Second})
		if (e == nil) != (mode == "pass") {
			t.Fatalf("mode %s: %v", mode, e)
		}
	}
	body, e := os.ReadFile(trace)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatal(string(body))
	}
	prior := ""
	for _, line := range lines {
		var row programRuntimeTrace
		if e = json.Unmarshal([]byte(line), &row); e != nil {
			t.Fatal(e)
		}
		if row.Home == prior {
			t.Fatal("zero-value runner reused unowned runtime")
		}
		prior = row.Home
		if _, e = os.Stat(filepath.Dir(row.Home)); !os.IsNotExist(e) {
			t.Fatal("per-call runtime leaked", e)
		}
	}
	entries, e := os.ReadDir(source)
	if e != nil || len(entries) != 0 {
		t.Fatalf("runtime wrote source: %v %v", entries, e)
	}
}
func TestProgramRuntimeCleanupReadOnlyModuleCache(t *testing.T) {
	root, e := newProgramRuntimeRoot(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	cache := filepath.Join(root, "go", "pkg", "mod", "example.invalid", "module@v1")
	if e = os.MkdirAll(cache, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(cache, "go.mod"), []byte("module cache\n"), 0444); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(cache, 0555); e != nil {
		t.Fatal(e)
	}
	if e = removeProgramRuntimeRoot(root); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("read-only cache leaked", e)
	}
}
