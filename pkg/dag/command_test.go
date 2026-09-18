// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeDAG(t *testing.T, md string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandListJSON(t *testing.T) {
	md := "## Tasks\n\n" +
		"### build\nCompile it.\nRequires: clean\n" + block("bash", "echo build") +
		"### clean\n" + block("bash", "echo clean")
	path := writeDAG(t, md)

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--list", "--json", "--file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	var env struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Result        struct {
			Tasks []struct {
				Name     string   `json:"name"`
				Desc     string   `json:"description"`
				Requires []string `json:"requires"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out.String())
	}
	if env.SchemaVersion != "dag-v1" || env.Command != "dag" || env.Status != "ok" {
		t.Errorf("envelope = %+v", env)
	}
	if len(env.Result.Tasks) != 2 || env.Result.Tasks[0].Name != "build" {
		t.Fatalf("tasks = %+v", env.Result.Tasks)
	}
	if env.Result.Tasks[0].Desc != "Compile it." {
		t.Errorf("desc = %q", env.Result.Tasks[0].Desc)
	}
}

func TestCommandNoArgListsByDefault(t *testing.T) {
	// No frontmatter default and no "default" target => no-arg lists targets
	// (like a Makefile whose .DEFAULT_GOAL is help), rather than running one.
	path := writeDAG(t, "## Tasks\n\n### danger\n"+block("bash", "echo SHOULD-NOT-RUN"))
	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out.String(), "SHOULD-NOT-RUN") {
		t.Errorf("no-arg invocation ran a target instead of listing; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "danger") {
		t.Errorf("no-arg invocation should list targets; out=%q", out.String())
	}
}

func TestCommandHelpMentionsWatchAndSandbox(t *testing.T) {
	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "--watch") {
		t.Fatalf("--help should mention --watch:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "--sandbox") {
		t.Fatalf("--help should mention --sandbox:\n%s", out.String())
	}
}

func TestCommandDefaultGoalFrontmatter(t *testing.T) {
	md := "---\ndefault: greet\n---\n\n## Tasks\n\n" +
		"### greet\n" + block("bash", "echo hello-default") +
		"### other\n" + block("bash", "echo other")
	path := writeDAG(t, md)
	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--file", path}) // no target -> default goal "greet"
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "hello-default") {
		t.Errorf("default goal not run; out=%q", out.String())
	}
}

func TestCommandVarOverride(t *testing.T) {
	// make-style: `dag echo NAME=world` injects NAME into the body's env.
	md := "## Tasks\n\n### echo\n" + block("bash", "echo \"hi ${NAME:-nobody}\"")
	path := writeDAG(t, md)
	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--file", path, "echo", "NAME=world"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "hi world") {
		t.Errorf("variable override not applied; out=%q", out.String())
	}
}

func TestCommandRunJSONSurfacesHost(t *testing.T) {
	md := "## Tasks\n\n### remote\nHost: host-a\n" + block("bash", "echo ok")
	path := writeDAG(t, md)

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json", "--file", path, "remote"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	var env struct {
		Result struct {
			Tasks []struct {
				Name string `json:"name"`
				Host string `json:"host"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(env.Result.Tasks) != 1 || env.Result.Tasks[0].Host != "host-a" {
		t.Fatalf("host not surfaced: %+v", env.Result.Tasks)
	}
}

func TestCommandExplain(t *testing.T) {
	// A target with no cache entry should be reported as "would run"; --explain
	// must not execute the body (no side-effect file appears).
	dir := t.TempDir()
	p := filepath.Join(dir, "DAG.md")
	md := "## Tasks\n\n### gen\nVenue: cloud\nDistribution: replicated\nGenerates: out.txt\n" + block("bash", "echo hi > ran.txt")
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--explain", "--json", "--file", p, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	var env struct {
		Status string `json:"status"`
		Result struct {
			Plan []struct {
				Name         string       `json:"name"`
				Venue        string       `json:"venue"`
				Distribution Distribution `json:"distribution"`
				WouldRun     bool         `json:"would_run"`
				Reason       string       `json:"reason"`
			} `json:"plan"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if env.Status != "ok" || len(env.Result.Plan) != 1 {
		t.Fatalf("plan = %+v", env.Result)
	}
	if !env.Result.Plan[0].WouldRun || env.Result.Plan[0].Reason == "" {
		t.Errorf("want would_run with a reason, got %+v", env.Result.Plan[0])
	}
	if env.Result.Plan[0].Venue != VenueCloud || env.Result.Plan[0].Distribution != DistributionReplicated {
		t.Errorf("placement metadata not explained: %+v", env.Result.Plan[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err == nil {
		t.Errorf("--explain executed the body (ran.txt created)")
	}
}

func TestCommandCachePortableImportExport(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // bodies run in the invoking cwd (make parity), not the file's dir
	p := filepath.Join(dir, "DAG.md")
	md := "## Tasks\n\n### gen\nSources: in.txt\nGenerates: out.txt\n" +
		block("bash", "cat in.txt > out.txt")
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	exportDir := filepath.Join(dir, "export")

	cmd := NewDagCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json", "--cache-dir", cacheDir, "--cache-export", exportDir, "--file", p, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}

	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd = NewDagCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json", "--cache-dir", cacheDir, "--cache-import", exportDir, "--file", p, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("second Execute: %v (stderr=%s)", err, errOut.String())
	}
	var env struct {
		Result struct {
			Tasks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(env.Result.Tasks) != 1 || env.Result.Tasks[0].Status != StatusUpToDate.String() {
		t.Fatalf("want imported cache to skip gen, got %+v", env.Result.Tasks)
	}
}

func TestCommandDryRun(t *testing.T) {
	// -n must print the ordered plan and run NO body (the side-effect file
	// must not appear).
	dir := t.TempDir()
	p := filepath.Join(dir, "DAG.md")
	md := "## Tasks\n\n" +
		"### prep\n" + block("bash", "echo prep > prep.txt") +
		"### build\nRequires: prep\nEffects: write\n" + block("bash", "echo build > build.txt")
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"-n", "--json", "--file", p, "build"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	var env struct {
		Status string `json:"status"`
		Result struct {
			Plan []struct {
				Name    string   `json:"name"`
				Command string   `json:"command"`
				Effects []string `json:"effects"`
			} `json:"plan"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if env.Status != "ok" || len(env.Result.Plan) != 2 {
		t.Fatalf("plan = %+v", env.Result.Plan)
	}
	// Topological order: prep before build.
	if env.Result.Plan[0].Name != "prep" || env.Result.Plan[1].Name != "build" {
		t.Errorf("plan order = %s, %s", env.Result.Plan[0].Name, env.Result.Plan[1].Name)
	}
	if env.Result.Plan[1].Command == "" {
		t.Errorf("plan should carry the first body line")
	}
	// Ran nothing: neither output file exists.
	for _, f := range []string{"prep.txt", "build.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("-n executed a body (%s created)", f)
		}
	}
}

func TestCommandOutputGroup(t *testing.T) {
	// --output-group -j N must emit exactly one ::group::/::endgroup:: pair per
	// target with that target's output between the markers.
	dir := t.TempDir()
	p := filepath.Join(dir, "DAG.md")
	md := "## Tasks\n\n" +
		"### a\n" + block("bash", "echo OUT-A") +
		"### b\nRequires: a\n" + block("bash", "echo OUT-B")
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--output-group", "-j", "4", "--plain", "--file", p, "b"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	s := out.String()
	if g := strings.Count(s, "::group::"); g != 2 {
		t.Errorf("want 2 ::group:: markers, got %d\n%s", g, s)
	}
	if e := strings.Count(s, "::endgroup::"); e != 2 {
		t.Errorf("want 2 ::endgroup:: markers, got %d\n%s", e, s)
	}
	// Each group's output sits between its markers: a's output appears after
	// "::group::a" and before the next "::endgroup::".
	ga := strings.Index(s, "::group::a")
	gb := strings.Index(s, "::group::b")
	if ga < 0 || gb < 0 || ga > gb {
		t.Fatalf("groups not in topological order: a@%d b@%d\n%s", ga, gb, s)
	}
	if oa := strings.Index(s, "OUT-A"); oa < ga {
		t.Errorf("OUT-A not inside group a\n%s", s)
	}
	if ob := strings.Index(s, "OUT-B"); ob < gb {
		t.Errorf("OUT-B not inside group b\n%s", s)
	}
}

func TestCommandCheck(t *testing.T) {
	// Valid file: --check reports ok and runs nothing.
	path := writeDAG(t, "## Tasks\n\n### a\n"+block("bash", "echo a > ran.txt")+
		"### b\nRequires: a\n"+block("bash", "echo b"))
	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--check", "--file", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("check valid: %v", err)
	}
	if !strings.Contains(out.String(), "ok:") {
		t.Errorf("check should report ok; got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "ran.txt")); err == nil {
		t.Errorf("--check executed a body")
	}

	// Cyclic file: --check fails with the state-conflict code.
	cyc := writeDAG(t, "## Tasks\n\n### x\nRequires: y\n"+block("bash", "echo x")+
		"### y\nRequires: x\n"+block("bash", "echo y"))
	cmd2 := NewDagCmd()
	cmd2.SetOut(new(bytes.Buffer))
	cmd2.SetErr(new(bytes.Buffer))
	cmd2.SetArgs([]string{"--check", "--file", cyc})
	if err := cmd2.Execute(); err == nil {
		t.Fatal("check should fail on a cycle")
	} else if ExitCodeOf(err) != 4 {
		t.Errorf("want exit 4 (state conflict), got %d", ExitCodeOf(err))
	}
}

func TestCommandUnknownTarget(t *testing.T) {
	path := writeDAG(t, "## Tasks\n\n### a\n"+block("bash", "echo a"))
	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json", "--file", path, "nope"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want error for unknown target")
	}
	if ExitCodeOf(err) != 2 {
		t.Errorf("want exit 2, got %d", ExitCodeOf(err))
	}
}

func TestCommandInjectsInvokingExecutable(t *testing.T) {
	path := writeDAG(t, "## Tasks\n\n### self\n"+block("bash", `printf '%s\n' "$BASHY"
printf '%s\n' "$BASHY_EXE"
printf '%s\n' "$BASHY_ARGV0"`))
	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--plain", "--force", "--file", path, "self"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dag self: %v; stderr=%s", err, errOut.String())
	}
	exe := resolveArgv0(os.Args[0])
	var got []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line != "" &&
			!strings.HasPrefix(line, "==> ") &&
			!strings.HasPrefix(line, "dag: ") &&
			!strings.Contains(line, "group::") &&
			!strings.Contains(line, "[group]") &&
			!strings.Contains(line, "[endgroup]") {
			got = append(got, line)
		}
	}
	want := []string{exe, exe, os.Args[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("self env mismatch:\n got: %q\nwant: %q\nfull output:\n%s", got, want, out.String())
	}
}

func TestCommandFolderArgResolvesEntry(t *testing.T) {
	// A directory positional must resolve to that folder's known entry (DAG.md),
	// so a conventional deploy folder like `.bashy/sdlc/deploy/` is the single
	// entry point: `bashy dag .bashy/sdlc/deploy deploy-qa`.
	dir := t.TempDir()
	md := "## Tasks\n\n### deploy-qa\n" + block("bash", "echo qa")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{dir, "--list", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "deploy-qa") {
		t.Fatalf("folder arg did not resolve to the entry: %s", out.String())
	}
}
