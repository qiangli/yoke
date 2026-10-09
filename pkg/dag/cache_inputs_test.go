// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Register the coreutils userland so bodies resolve in-process.
	_ "github.com/qiangli/coreutils/cmds/all"
)

func inputsNode(t *testing.T, md, name string) *Node {
	t.Helper()
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	n, ok := g.Nodes[name]
	if !ok {
		t.Fatalf("no node %q", name)
	}
	return n
}

func TestFingerprintEnvChangeInvalidates(t *testing.T) {
	mk := func(env string) *Node {
		md := "## Tasks\n\n### build\nEnv: " + env + "\nGenerates: out.txt\n" +
			block("bash", "echo $FOO > out.txt")
		return inputsNode(t, md, "build")
	}
	c := &Cache{Hashes: map[string]string{}}
	dir := t.TempDir()
	fp1 := c.Fingerprint(mk("FOO=1"), dir, nil)
	fp2 := c.Fingerprint(mk("FOO=2"), dir, nil)
	if fp1 == fp2 {
		t.Fatal("changing Task.Env did not change the fingerprint")
	}
	if fp3 := c.Fingerprint(mk("FOO=1"), dir, nil); fp3 != fp1 {
		t.Fatal("same Task.Env did not produce the same fingerprint")
	}
}

func TestFingerprintHostLangInvalidate(t *testing.T) {
	dir := t.TempDir()
	base := "## Tasks\n\n### build\nGenerates: out.txt\n" + block("bash", "echo hi > out.txt")
	hosted := "## Tasks\n\n### build\nHost: gpu-1\nGenerates: out.txt\n" + block("bash", "echo hi > out.txt")
	c := &Cache{Hashes: map[string]string{}}
	fpBase := c.Fingerprint(inputsNode(t, base, "build"), dir, nil)
	if fpHost := c.Fingerprint(inputsNode(t, hosted, "build"), dir, nil); fpHost == fpBase {
		t.Fatal("changing Host did not change the fingerprint")
	}
	// ``` (default) and ```bash run the same interpreter: same fingerprint.
	bare := "## Tasks\n\n### build\nGenerates: out.txt\n" + block("", "echo hi > out.txt")
	if fpBare := c.Fingerprint(inputsNode(t, bare, "build"), dir, nil); fpBare != fpBase {
		t.Fatal("default lang and bash lang produced different fingerprints")
	}
}

func TestFingerprintMatrixChildrenDiffer(t *testing.T) {
	md := "## Tasks\n\n### build\nMatrix: os=linux,darwin\nGenerates: out.txt\n" +
		block("bash", "echo $os > out.txt")
	d := doc(t, md)
	d.expandMatrix()
	g, err := BuildGraph(d)
	if err != nil {
		t.Fatal(err)
	}
	c := &Cache{Hashes: map[string]string{}}
	dir := t.TempDir()
	fpLinux := c.Fingerprint(g.Nodes["build:os=linux"], dir, nil)
	fpDarwin := c.Fingerprint(g.Nodes["build:os=darwin"], dir, nil)
	if fpLinux == fpDarwin {
		t.Fatal("matrix children with different Env share a fingerprint")
	}
}

func TestFingerprintExecInputsOverride(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### build\nGenerates: out.txt\n" +
		block("bash", "echo $VERSION > out.txt")
	n := inputsNode(t, md, "build")

	withInputs := func(vars map[string]string) string {
		c := &Cache{Hashes: map[string]string{}, ExecInputs: vars}
		return c.Fingerprint(n, dir, nil)
	}
	fp1 := withInputs(map[string]string{"VERSION": "1"})
	if fp2 := withInputs(map[string]string{"VERSION": "2"}); fp2 == fp1 {
		t.Fatal("changing a resolved frontmatter/CLI var did not change the fingerprint")
	}
	if fp3 := withInputs(map[string]string{"VERSION": "1"}); fp3 != fp1 {
		t.Fatal("same resolved inputs did not produce the same fingerprint")
	}
	// Target Env wins over document-level inputs.
	c := &Cache{Hashes: map[string]string{}, ExecInputs: map[string]string{"FOO": "doc"}}
	fpDoc := c.Fingerprint(n, dir, nil)
	n.Task.Env = []string{"FOO=target"}
	if fpTarget := c.Fingerprint(n, dir, nil); fpTarget == fpDoc {
		t.Fatal("target Env did not override document-level inputs in the fingerprint")
	}
}

func TestFingerprintValuesNeverStored(t *testing.T) {
	dir := t.TempDir()
	const secret = "s3cr3t-value-that-must-not-leak"
	c := &Cache{
		Hashes:     map[string]string{},
		ExecInputs: map[string]string{"TOKEN": secret},
	}
	c.path = filepath.Join(dir, "cache.json")
	md := "## Tasks\n\n### build\nEnv: PASSWORD=" + secret + "\nGenerates: out.txt\n" +
		block("bash", "echo hi > out.txt")
	n := inputsNode(t, md, "build")
	c.Record(n.Task.Name, c.Fingerprint(n, dir, nil))
	c.Save()
	data, err := os.ReadFile(filepath.Join(dir, "cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("cache file contains a raw execution-input value")
	}
}

func TestEngineBodyOnlyVarChangeRebuilds(t *testing.T) {
	dir := t.TempDir()
	sharedInputsCache := &Cache{Hashes: map[string]string{}}
	// GREETING is used only in the body: no Sources/Inputs mention it.
	mk := func(env string) *Engine {
		md := "## Tasks\n\n### gen\nEnv: " + env + "\nGenerates: out.txt\n" +
			block("bash", "echo $GREETING > out.txt")
		g, err := BuildGraph(doc(t, md))
		if err != nil {
			t.Fatal(err)
		}
		return &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1,
			FailFast: true, Cache: sharedInputsCache, Stdout: io.Discard, Stderr: io.Discard}
	}

	r1, err := mk("GREETING=hello").Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Results[0].Status != StatusDone {
		t.Fatalf("first run: want done, got %s", r1.Results[0].Status)
	}
	r2, err := mk("GREETING=hello").Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Results[0].Status != StatusUpToDate {
		t.Fatalf("unchanged inputs rerun: want up-to-date, got %s", r2.Results[0].Status)
	}
	r3, err := mk("GREETING=hi").Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r3.Results[0].Status != StatusDone {
		t.Fatalf("changed body-only var: want done, got %s", r3.Results[0].Status)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(data)) != "hi" {
		t.Fatalf("out.txt = %q, want rebuilt content", string(data))
	}
}
