// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const makefileFixture = "## Tasks\n\n" +
	"### build\nCompile it.\n\n```bash\necho build\n```\n\n" +
	"### test\nTest it.\nRequires: build\n\n```bash\necho test\n```\n"

const wantMakefileGuard = `command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }`

func TestMakefileGolden(t *testing.T) {
	doc, err := Parse(strings.NewReader(makefileFixture), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := string(renderMakefile(doc, "bashy dag"))
	want := "###########################################################################\n" +
		"##  GENERATED FILE — DO NOT EDIT.                                        ##\n" +
		"##  Produced from dag.md by `bashy dag --makefile`. Edit dag.md instead, ##\n" +
		"##  then regenerate; manual changes are overwritten and fail --check.    ##\n" +
		"###########################################################################\n" +
		"# Tasks live in dag.md (a bashy dag file); this Makefile only forwards.\n" +
		"#   bashy dag <target>           run a target (its dependencies first)\n" +
		"#   bashy dag --list             list targets\n" +
		"#   bashy dag --dry-run <target> print the plan, run nothing\n" +
		"#   bashy dag --skill DIR        export these tasks as an Agent Skill\n" +
		"# Regenerate with: bashy dag --makefile  (verify: bashy dag --makefile --check)\n" +
		"\n" +
		"# List the dag targets, then how to run them natively.\n" +
		"help: FORCE\n" +
		"\t@" + wantMakefileGuard + "; bashy dag --list; echo \"Run: bashy dag <target> | skill: bashy dag --skill DIR | more: bashy help\"\n" +
		"\n" +
		"# Compile it.\n" +
		"build: FORCE\n" +
		"\t@" + wantMakefileGuard + "; bashy dag build\n" +
		"\n" +
		"# Test it.\n" +
		"test: FORCE\n" +
		"\t@" + wantMakefileGuard + "; bashy dag test\n" +
		"\n" +
		".DEFAULT: ; @" + wantMakefileGuard + "; bashy dag $@\n" +
		"\n" +
		"# FORCE names no file and has no recipe: as the one prerequisite it\n" +
		"# keeps every forward running even when a file or directory on disk\n" +
		"# shares a target's name. Dependency order stays in dag, not here.\n" +
		"FORCE:\n"
	if got != want {
		t.Errorf("renderMakefile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A dag target named help replaces the fixed help rule, and is still first
// (bare `make` runs the first rule in both POSIX and GNU make).
func TestMakefileHelpPrecedence(t *testing.T) {
	md := "## Tasks\n\n### build\nCompile it.\n\n```bash\necho build\n```\n\n" +
		"### help\nShow usage.\n\n```bash\necho usage\n```\n"
	doc, err := Parse(strings.NewReader(md), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := string(renderMakefile(doc, "bashy dag"))
	if n := strings.Count(got, "\nhelp: FORCE\n"); n != 1 {
		t.Errorf("help rules = %d, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "; bashy dag help\n") {
		t.Errorf("dag help not forwarded:\n%s", got)
	}
	if strings.Contains(got, "bashy dag --list;") {
		t.Errorf("fixed help leaked in beside the dag help target:\n%s", got)
	}
	if i, j := strings.Index(got, "\nhelp: FORCE\n"), strings.Index(got, "\nbuild: FORCE\n"); i < 0 || j < 0 || i > j {
		t.Errorf("help is not the first rule (help@%d build@%d):\n%s", i, j, got)
	}
}

// A target with no description gets no comment line, not an empty one.
func TestMakefileNoDescNoComment(t *testing.T) {
	md := "## Tasks\n\n### clean\n\n```bash\necho clean\n```\n"
	doc, err := Parse(strings.NewReader(md), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := string(renderMakefile(doc, "bashy dag"))
	if !strings.Contains(got, "\n\nclean: FORCE\n") {
		t.Errorf("want bare clean rule:\n%s", got)
	}
}

func runDagCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewDagCmd()
	var b bytes.Buffer
	cmd.SetOut(&b)
	cmd.SetErr(&b)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return b.String(), err
}

func TestMakefileCLIWritesAndRunsNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	md := "## Tasks\n\n### build\nCompile it.\n\n```bash\necho ran > ran.txt\n```\n"
	if err := os.WriteFile(filepath.Join(dir, "dag.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runDagCmd(t, "--makefile")
	if err != nil {
		t.Fatalf("--makefile: %v\n%s", err, out)
	}
	if !strings.Contains(out, "makefile: Makefile") {
		t.Errorf("output = %q", out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "; bashy dag build\n") {
		t.Errorf("Makefile missing the build forward:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); !os.IsNotExist(err) {
		t.Error("--makefile ran a target body")
	}

	// Regeneration overwrites its own output.
	if out, err := runDagCmd(t, "--makefile"); err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
}

func TestMakefileCLIRefusesHandWritten(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "dag.md"), []byte(makefileFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := "all:\n\techo mine\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runDagCmd(t, "--makefile")
	if err == nil {
		t.Fatalf("clobbered a hand-written Makefile:\n%s", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Makefile"))
	if string(b) != mine {
		t.Errorf("hand-written Makefile changed:\n%s", b)
	}
}

func TestMakefileCheckStale(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "dag.md"), []byte(makefileFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	// Missing Makefile: check fails and says to regenerate.
	out, err := runDagCmd(t, "--makefile", "--check")
	if err == nil || !strings.Contains(out, "regenerate") {
		t.Fatalf("missing-file check: err=%v out=%q", err, out)
	}
	// Fresh: check passes.
	if out, err := runDagCmd(t, "--makefile"); err != nil {
		t.Fatalf("--makefile: %v\n%s", err, out)
	}
	if out, err := runDagCmd(t, "--makefile", "--check"); err != nil {
		t.Fatalf("fresh check: %v\n%s", err, out)
	}
	// Stale: an extra line fails the check.
	f, err := os.OpenFile(filepath.Join(dir, "Makefile"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("# hand edit\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err = runDagCmd(t, "--makefile", "--check")
	if err == nil || !strings.Contains(out, "regenerate") {
		t.Fatalf("stale check: err=%v out=%q", err, out)
	}
	// Plain --check still validates the dag file (unchanged meaning).
	if out, err := runDagCmd(t, "--check"); err != nil {
		t.Fatalf("plain --check: %v\n%s", err, out)
	}
}

func TestMakefileOutputAndFileFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "dag.md"), []byte(makefileFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	// Output in dir, dag under sub/: recipes must name the dag file.
	if out, err := runDagCmd(t, "-f", "sub/dag.md", "--makefile", "-o", "out.mk"); err != nil {
		t.Fatalf("--makefile -o: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.mk"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "; bashy dag -f sub/dag.md build\n") {
		t.Errorf("want -f sub/dag.md recipes:\n%s", b)
	}
	if strings.Contains(string(b), dir) {
		t.Errorf("Makefile leaks the host path %s:\n%s", dir, b)
	}
	// Output beside the dag file: plain "bashy dag" recipes again.
	if out, err := runDagCmd(t, "-f", "sub/dag.md", "--makefile", "-o", "sub/Makefile"); err != nil {
		t.Fatalf("--makefile beside: %v\n%s", err, out)
	}
	b, err = os.ReadFile(filepath.Join(sub, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "; bashy dag build\n") || strings.Contains(string(b), "bashy dag -f") {
		t.Errorf("want plain bashy dag recipes beside dag.md:\n%s", b)
	}
}

// The banner frame is measured in runes, not bytes (its em dash is one
// rune, three bytes): every banner line is the same rune width.
func TestMakefileBannerRuneWidths(t *testing.T) {
	doc, err := Parse(strings.NewReader(makefileFixture), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lines := strings.Split(string(renderMakefile(doc, "bashy dag")), "\n")
	if len(lines) < 5 {
		t.Fatalf("generated text too short: %q", lines)
	}
	for i := 0; i < 5; i++ {
		if w := utf8.RuneCountInString(lines[i]); w != 75 {
			t.Errorf("banner line %d is %d runes, want 75: %q", i+1, w, lines[i])
		}
	}
}

// A directory on disk that shares a target's name must not read as
// up-to-date: the FORCE prerequisite keeps the forward running. Exercised
// under every make on PATH — GNU make and bashy's coreutils make — with a
// fake bashy first on PATH so the forward is observable and runs nothing.
func TestMakefileForwardsWhenTargetNameExistsOnDisk(t *testing.T) {
	realBashy, bashyErr := exec.LookPath("bashy")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dag.md"), []byte(makefileFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if out, err := runDagCmd(t, "--makefile"); err != nil {
		t.Fatalf("--makefile: %v\n%s", err, out)
	}
	if err := os.Mkdir(filepath.Join(dir, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\necho FAKE-BASHY \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "bashy"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	type makeRun struct {
		name string
		argv []string
	}
	var runs []makeRun
	for _, cand := range []string{"gmake", "make"} {
		if p, err := exec.LookPath(cand); err == nil {
			runs = append(runs, makeRun{"gnu:" + p, []string{p, "test"}})
			break
		}
	}
	if bashyErr == nil {
		runs = append(runs, makeRun{"coreutils:" + realBashy, []string{realBashy, "make", "test"}})
	}
	if len(runs) == 0 {
		t.Skip("no make binary on PATH")
	}
	for _, r := range runs {
		cmd := exec.Command(r.argv[0], r.argv[1:]...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		t.Logf("%s -> err=%v out=%q", r.name, err, out)
		if err != nil {
			t.Errorf("%s: %v\n%s", r.name, err, out)
			continue
		}
		if !strings.Contains(string(out), "FAKE-BASHY dag test") {
			t.Errorf("%s: forward did not run with a test/ directory present:\n%s", r.name, out)
		}
	}
}

func TestMakefileJSON(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "dag.md"), []byte(makefileFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runDagCmd(t, "--json", "--makefile")
	if err != nil {
		t.Fatalf("--json --makefile: %v\n%s", err, out)
	}
	for _, want := range []string{`"makefile": "Makefile"`, `"targets": 2`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %s:\n%s", want, out)
		}
	}
}
