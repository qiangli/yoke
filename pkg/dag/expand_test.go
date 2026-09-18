// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"sort"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

// expandedNames runs the parse+expand passes and returns the resulting task
// order, so a test can assert the matrix fan-out shape directly.
func expandedNames(t *testing.T, md string, overrides ...string) []string {
	t.Helper()
	d := doc(t, md)
	d.expandVars(os.Environ(), overrides)
	d.expandMatrix()
	return append([]string(nil), d.Order...)
}

func TestMatrixExpansion(t *testing.T) {
	md := "## Tasks\n\n### build\nMatrix: os=linux,darwin\n" + block("bash", "echo $os")
	names := expandedNames(t, md)
	got := append([]string(nil), names...)
	sort.Strings(got)
	want := []string{"build", "build:os=darwin", "build:os=linux"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded order = %v, want superset %v", names, want)
	}

	// The aggregator 'build' Requires both expansions.
	d := doc(t, md)
	d.expandMatrix()
	agg, ok := d.Lookup("build")
	if !ok {
		t.Fatal("aggregator 'build' missing")
	}
	wantReq := []string{"build:os=darwin", "build:os=linux"}
	gotReq := append([]string(nil), agg.Requires...)
	sort.Strings(gotReq)
	if !reflect.DeepEqual(gotReq, wantReq) {
		t.Errorf("aggregator requires = %v, want %v", agg.Requires, wantReq)
	}
	if agg.Body != "" {
		t.Errorf("aggregator should be phony, body = %q", agg.Body)
	}
}

func TestMatrixMultiKeyCartesian(t *testing.T) {
	md := "## Tasks\n\n### build\nMatrix: os=linux,darwin arch=amd64,arm64\n" + block("bash", "true")
	d := doc(t, md)
	d.expandMatrix()
	// 2x2 = 4 concrete nodes + 1 aggregator.
	if len(d.Tasks) != 5 {
		t.Fatalf("want 5 tasks (4 combos + aggregator), got %d: %v", len(d.Tasks), d.Order)
	}
	want := "build:arch=amd64,os=linux"
	if _, ok := d.Lookup(want); !ok {
		t.Errorf("missing deterministic combo %q in %v", want, d.Order)
	}
}

func TestVarsExpandMetadata(t *testing.T) {
	md := "---\nvars:\n  BIN: app\n---\n\n## Tasks\n\n### t\nGenerates: bin/${BIN}\n" +
		block("bash", "true")
	d := doc(t, md)
	if len(d.Vars) != 1 || d.Vars[0].Name != "BIN" || d.Vars[0].Value != "app" {
		t.Fatalf("vars = %+v", d.Vars)
	}
	d.expandVars(nil, nil)
	got, _ := d.Lookup("t")
	if !reflect.DeepEqual(got.Generates, []string{"bin/app"}) {
		t.Errorf("Generates = %v, want [bin/app]", got.Generates)
	}
}

func TestVarsResolvedForBodyEnv(t *testing.T) {
	// expandVars returns the doc's resolved vars so the command layer can inject
	// them into body env (frontmatter vars reach bodies, not just metadata).
	md := "---\nvars:\n  HOST: host-b\n  REF: main\n---\n## Tasks\n\n### t\n" + block("bash", "true")
	d := doc(t, md)
	got := d.expandVars(nil, []string{"REF=dev"}) // CLI override wins
	if got["HOST"] != "host-b" {
		t.Errorf("HOST resolved = %q", got["HOST"])
	}
	if got["REF"] != "dev" {
		t.Errorf("REF should reflect CLI override, got %q", got["REF"])
	}
}

func TestVarsExpandHostWhenEnsure(t *testing.T) {
	// ${NAME} expands in Host (placement), When (condition), and Ensure
	// (postconditions) — not just the slice metadata.
	md := "## Tasks\n\n### remote\n" +
		"Host: ${HOST}\n" +
		"When: test -n \"${HOST}\"\n" +
		"Ensure: file-exists ${HOST}.done\n" +
		block("bash", "true")
	d := doc(t, md)
	d.expandVars(nil, []string{"HOST=bigbox"})
	got, _ := d.Lookup("remote")
	if got.Host != "bigbox" {
		t.Errorf("Host = %q, want bigbox", got.Host)
	}
	if got.When != `test -n "bigbox"` {
		t.Errorf("When = %q", got.When)
	}
	if !reflect.DeepEqual(got.Ensure, []string{"file-exists bigbox.done"}) {
		t.Errorf("Ensure = %v", got.Ensure)
	}
}

func TestVarsCLIOverridesWin(t *testing.T) {
	md := "---\nvars:\n  BIN: app\n---\n\n## Tasks\n\n### t\nGenerates: bin/${BIN}\n" +
		block("bash", "true")
	d := doc(t, md)
	d.expandVars(nil, []string{"BIN=x"})
	got, _ := d.Lookup("t")
	if !reflect.DeepEqual(got.Generates, []string{"bin/x"}) {
		t.Errorf("CLI override: Generates = %v, want [bin/x]", got.Generates)
	}
}

func TestVarsDefaultIfUnsetSemantics(t *testing.T) {
	// ?= keeps an existing (process-env) value; = overrides it.
	md := "---\nvars:\n  A ?= fromvar\n  B = fromvar\n---\n\n## Tasks\n\n### t\n" +
		"Generates: ${A}/${B}\n" + block("bash", "true")
	d := doc(t, md)
	d.expandVars([]string{"A=fromenv", "B=fromenv"}, nil)
	got, _ := d.Lookup("t")
	if !reflect.DeepEqual(got.Generates, []string{"fromenv/fromvar"}) {
		t.Errorf("Generates = %v, want [fromenv/fromvar] (?= keeps env, = overrides)", got.Generates)
	}
}

func TestVarsEndToEndThroughCommand(t *testing.T) {
	// Full path: frontmatter vars expand in a body via the injected env, and a
	// CLI override wins, proving expandVars is wired into the command.
	md := "---\nvars:\n  BIN: app\n---\n\n## Tasks\n\n### show\nEnv: NAME=${BIN}\n" +
		block("bash", "echo \"name=${NAME}\"")
	path := writeDAG(t, md)
	run := func(args ...string) string {
		cmd := NewDagCmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append([]string{"--plain", "--file", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out.String()
	}
	if s := run("show"); !contains(s, "name=app") {
		t.Errorf("vars default not applied; out=%q", s)
	}
	if s := run("show", "BIN=zed"); !contains(s, "name=zed") {
		t.Errorf("CLI override not applied; out=%q", s)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

func TestMatrixRunsEachCombination(t *testing.T) {
	dir := t.TempDir()
	// Each combination writes a file named after its injected $os value, proving
	// the matrix value reached the body's environment.
	md := "## Tasks\n\n### build\nMatrix: os=linux,darwin\n" +
		block("bash", "touch built-$os")
	d := doc(t, md)
	d.expandMatrix()
	g, err := BuildGraph(d)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "build")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("matrix run failed: %+v", report.Results)
	}
	for _, name := range []string{"built-linux", "built-darwin"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Errorf("%s not created — matrix value not injected into env", name)
		}
	}
}
