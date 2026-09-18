// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

func TestArtifactsRecordedInResult(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### make\nArtifacts: out.txt\n" +
		block("bash", "echo hi > out.txt")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "make")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := report.Results[0].Artifacts; !reflect.DeepEqual(got, []string{"out.txt"}) {
		t.Errorf("artifacts = %v, want [out.txt]", got)
	}
}

func TestArtifactsInJSONEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // bodies run in the invoking cwd (make parity)
	p := filepath.Join(dir, "DAG.md")
	md := "## Tasks\n\n### make\nArtifacts: out.txt\n" + block("bash", "echo hi > out.txt")
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json", "--file", p, "make"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	var env struct {
		Result struct {
			Tasks []struct {
				Name      string   `json:"name"`
				Artifacts []string `json:"artifacts"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(env.Result.Tasks) != 1 || !reflect.DeepEqual(env.Result.Tasks[0].Artifacts, []string{"out.txt"}) {
		t.Fatalf("artifacts in envelope = %+v", env.Result.Tasks)
	}
}

func TestArtifactsCopiedToDir(t *testing.T) {
	dir := t.TempDir()
	dest := t.TempDir()
	md := "## Tasks\n\n### make\nArtifacts: out.txt\n" + block("bash", "echo hi > out.txt")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &Engine{Graph: g, Dir: dir,
		Env:         append(os.Environ(), "DAG_ARTIFACTS_DIR="+dest),
		Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	if _, err := eng.Run(context.Background(), "make"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "out.txt")); err != nil {
		t.Errorf("artifact not copied to $DAG_ARTIFACTS_DIR: %v", err)
	}
}

func TestArtifactsGlob(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### make\nArtifacts: *.log\n" +
		block("bash", "echo a > a.log\necho b > b.log")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "make")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := report.Results[0].Artifacts; !reflect.DeepEqual(got, []string{"a.log", "b.log"}) {
		t.Errorf("glob artifacts = %v, want [a.log b.log]", got)
	}
}
