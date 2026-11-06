package shape

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/fleet"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

func TestProgramGoDirectoryArgumentAllowsProductionEditAndGeneratedRuntimeData(t *testing.T) {
	s, caller, input, _, _ := testService(t)
	files := map[string]string{
		"net/server/go.mod":  "module guardednested\n\ngo 1.20\n",
		"net/server/main.go": "package main\nconst greeting=\"base\"\nfunc main(){}\n",
		"net/server/main_test.go": `package main
import("os";"testing")
func TestMain(m *testing.M){
 if err:=os.MkdirAll("testdata",0700);err!=nil{panic(err)}
 if err:=os.WriteFile("testdata/generated.db",[]byte("runtime state"),0600);err!=nil{panic(err)}
 os.Exit(m.Run())
}
func TestBaseline(t *testing.T){if greeting==""{t.Fatal("missing greeting")}}
`,
		"net/server/testdata/frozen.fixture": "trusted fixture",
		"gate.sh":                            "trusted gate wrapper",
	}
	repo, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		t.Fatal(e)
	}
	wt, _ := repo.Worktree()
	for name, body := range files {
		p := filepath.Join(s.App.SourceDir, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = wt.Add(name); e != nil {
			t.Fatal(e)
		}
	}
	base, e := wt.Commit("native nested fixture", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
	if e != nil {
		t.Fatal(e)
	}
	s.App.Shaping = &fleet.AppShaping{Kind: "cli", Build: []string{"go", "-C", "net/server", "build", "-o", "{output}", "."}, Test: []string{"go", "-C", "net/server", "test", "-json", "-count=1", "./..."}}
	s.Options.Verifier = nil
	s.Options.Activator = nil
	executions := 0
	s.Options.ProgramRunner = shapeverify.ProgramRunnerFunc(func(ctx context.Context, r shapeverify.ProgramRun) error {
		executions++
		cmd := exec.CommandContext(ctx, r.Argv[0], r.Argv[1:]...)
		cmd.Dir = r.Dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
		output, e := cmd.CombinedOutput()
		if r.Output != nil {
			*r.Output = string(output)
		}
		return e
	})
	guards, e := s.programGuard(s.App.SourceDir, base.String())
	if e != nil {
		t.Fatal(e)
	}
	if slices.Contains(guards, "net/server") {
		t.Fatal("directory argument froze whole Go module")
	}
	for _, name := range []string{"net/server/go.mod", "net/server/main_test.go", "net/server/testdata/frozen.fixture"} {
		if !slices.Contains(guards, name) {
			t.Fatalf("baseline criterion not frozen: %s", name)
		}
	}
	explicit := s
	explicit.App.Protected = []string{"net/server"}
	explicitGuards, e := explicit.programGuard(s.App.SourceDir, base.String())
	if e != nil || !slices.Contains(explicitGuards, "net/server") {
		t.Fatal("explicit protected directory lost", e)
	}
	gateApp := s
	policy := *s.App.Shaping
	policy.Build = []string{"sh", "gate.sh"}
	gateApp.App.Shaping = &policy
	gateGuards, e := gateApp.programGuard(s.App.SourceDir, base.String())
	if e != nil || !slices.Contains(gateGuards, "gate.sh") {
		t.Fatalf("actual argv file not frozen: %v %v", gateGuards, e)
	}
	if e = CopyWorkspace(s.App.SourceDir, input); e != nil {
		t.Fatal(e)
	}
	main := filepath.Join(input, "net", "server", "main.go")
	if e = os.WriteFile(main, []byte(strings.Replace(files["net/server/main.go"], `"base"`, `"shaped"`, 1)), 0600); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	candidate, e := s.Import(ctx, caller, Wish{Request: "change production greeting inside module"}, input)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := s.Verify(ctx, caller, candidate.ID)
	if e != nil || verified.Verdict == nil || !verified.Verdict.Pass {
		t.Fatalf("ordinary source/runtime data refused: %+v %v", verified.Verdict, e)
	}
	for _, root := range []string{s.App.SourceDir, candidate.Workspace} {
		if _, e = os.Stat(filepath.Join(root, "net", "server", "testdata", "generated.db")); !os.IsNotExist(e) {
			t.Fatal("verification runtime output escaped isolated clone", root, e)
		}
	}
	t.Run("runtime cannot overwrite tracked fixture", func(t *testing.T) {
		from := t.TempDir()
		if e = CopyWorkspace(s.App.SourceDir, from); e != nil {
			t.Fatal(e)
		}
		code := "package main\nimport \"os\"\nconst greeting=\"shaped\"\nfunc init(){if err:=os.WriteFile(\"testdata/frozen.fixture\",[]byte(\"changed\"),0600);err!=nil{panic(err)}}\nfunc main(){}\n"
		if e = os.WriteFile(filepath.Join(from, "net", "server", "main.go"), []byte(code), 0600); e != nil {
			t.Fatal(e)
		}
		c, e := s.Import(ctx, caller, Wish{Request: "runtime fixture mutation"}, from)
		if e != nil {
			t.Fatal(e)
		}
		before := executions
		v, e := s.Verify(ctx, caller, c.ID)
		if !errors.Is(e, ErrVerification) || v.Verdict == nil || !strings.Contains(v.Verdict.Detail, "build/test changed tracked source") || executions <= before {
			t.Fatalf("runtime tracked mutation accepted: %+v %v", v.Verdict, e)
		}
	})
	for _, name := range []string{"net/server/main_test.go", "net/server/testdata/frozen.fixture", "net/server/go.mod"} {
		t.Run(name, func(t *testing.T) {
			from := t.TempDir()
			if e = CopyWorkspace(s.App.SourceDir, from); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(from, filepath.FromSlash(name)), []byte("candidate changed criterion"), 0600); e != nil {
				t.Fatal(e)
			}
			c, e := s.Import(ctx, caller, Wish{Request: "tamper frozen source"}, from)
			if e != nil {
				t.Fatal(e)
			}
			before := executions
			v, e := s.Verify(ctx, caller, c.ID)
			if !errors.Is(e, ErrVerification) || v.Verdict == nil || v.Verdict.Pass {
				t.Fatalf("frozen input accepted: %+v %v", v.Verdict, e)
			}
			if executions != before {
				t.Fatal("criterion tampering reached build/test")
			}
		})
	}
}
