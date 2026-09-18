// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A ```bashpp body runs under the Bash++ dialect: Go-shaped declarations are
// statements, not a parse error. A ```bash body is still Classic.
func TestBashppBodyRunsAsBashPP(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	md := "## Tasks\n\n### typed\n" + block("bashpp", "x := 40\ny := 2\necho \"sum=$((x + y))\"") +
		"### classic\n" + block("bash", "echo classic")
	path := writeDAG(t, md)

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--file", path, "typed", "classic"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "sum=42") || !strings.Contains(out.String(), "classic") {
		t.Fatalf("out=%q", out.String())
	}
}

// A top-level ```python body has no interpreter: the way to reach Python is a
// `~~~py` fence INSIDE a ```bashpp body, not a body language of its own.
func TestPythonBodyLanguageIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeDAG(t, "## Tasks\n\n### py\n"+block("python", "print(1)"))
	cmd := NewDagCmd()
	errOut := new(bytes.Buffer)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--file", path, "py"})
	if err := cmd.Execute(); err == nil || !strings.Contains(errOut.String(), `no interpreter for language "python"`) {
		t.Fatalf("err=%v stderr=%q", err, errOut.String())
	}
}

// The documented launcher shape: a ```bashpp body declares `~~~py as py` and
// calls py.main(); the value comes back into the body. Needs a python3.
func TestBashppBodyPyFenceLauncher(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	body := strings.Join([]string{
		"~~~py as py",
		"def main() -> str:",
		"    import os",
		"    return 'launched:' + os.path.basename(os.getcwd())",
		"~~~",
		"value := py.main()",
		`echo "$value"`,
		`[ "$value" = "launched:` + filepath.Base(dir) + `" ]`,
	}, "\n")
	path := writeDAG(t, "## Tasks\n\n### smoke\n"+block("bashpp", body))

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--file", path, "smoke"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "launched:"+filepath.Base(dir)) {
		t.Fatalf("out=%q", out.String())
	}
}

// Bodies run in the INVOKING cwd (make parity): -f only picks the file. A task
// file kept in another directory acts on the directory you stand in, and its
// Sources: fingerprint resolves there too. --explain reports the effective dir.
func TestBodiesRunInInvokingCwd(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	elsewhere := t.TempDir()
	path := filepath.Join(elsewhere, "pipeline.md")
	md := "## Tasks\n\n### gen\nSources: in.txt\nGenerates: out.txt\n" + block("bash", "cat in.txt > out.txt; pwd")
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "in.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--explain", "--json", "--file", path, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("explain: %v (stderr=%s)", err, errOut.String())
	}
	var env struct {
		Result struct {
			Dir string `json:"dir"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if got, want := mustAbs(t, env.Result.Dir), mustAbs(t, work); got != want {
		t.Fatalf("explain dir = %q, want %q", got, want)
	}

	cmd = NewDagCmd()
	out, errOut = new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--file", path, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if b, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(b) != "seed" {
		t.Fatalf("out.txt in cwd: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "out.txt")); err == nil {
		t.Fatal("body ran in the task file's directory, not the invoking cwd")
	}
}

// The one exception: a positional DIRECTORY names both the file and the place
// to run it, so `bashy dag .bashy/deploy target` keeps working from anywhere.
func TestPositionalDirectoryRunsThere(t *testing.T) {
	t.Chdir(t.TempDir())
	target := t.TempDir()
	md := "## Tasks\n\n### gen\n" + block("bash", "echo made > made.txt")
	if err := os.WriteFile(filepath.Join(target, "dag.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewDagCmd()
	errOut := new(bytes.Buffer)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{target, "gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(target, "made.txt")); err != nil {
		t.Fatalf("positional dir did not run there: %v", err)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}
