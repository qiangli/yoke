package dag

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/stream"
)

// dag --graph is the task graph as a bashy-graph-v1 stream: deterministic,
// independent of where the checkout lives, and it runs nothing.
func TestCommandGraphStream(t *testing.T) {
	md := "---\nname: demo\ndefault: build\n---\n## Tasks\n\n" +
		"### build\nCompile it.\nRequires: clean, gen\n" + block("bash", "touch built") +
		"### gen\nSources: a.go\n" + block("bash", "echo gen") +
		"### clean\n" + block("bash", "echo clean")
	run := func(dir string, args ...string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		cmd := NewDagCmd()
		out, errOut := new(bytes.Buffer), new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(errOut)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v %s", args, err, errOut)
		}
		if _, err := os.Stat(filepath.Join(dir, "built")); err == nil {
			t.Fatal("--graph ran a target body")
		}
		return out.String()
	}
	a, b := t.TempDir(), t.TempDir()
	first := run(a, "--graph")
	if again := run(a, "--graph"); again != first {
		t.Fatal("not deterministic")
	}
	if other := run(b, "--graph", "--file", filepath.Join(b, "DAG.md")); other != first {
		t.Fatalf("bytes depend on the checkout location:\n%s\n---\n%s", first, other)
	}
	// --json does not change the stream (it is not an envelope).
	if j := run(a, "--graph", "--json"); j != first {
		t.Fatalf("--json changed --graph output")
	}
	g, err := stream.Decode(strings.NewReader(first), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, first)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if g.Source != "dag:DAG.md" || g.Cursor.Binding.Provider != "dag" || g.Cursor.Binding.Query != "op=graph" {
		t.Fatalf("binding: %q %+v", g.Source, g.Cursor.Binding)
	}
	var requires []string
	for _, e := range g.Edges {
		if e.Rel == "requires" {
			requires = append(requires, e.Src+" -> "+e.Dst)
		}
	}
	if want := "dag:DAG.md!/build -> dag:DAG.md!/clean,dag:DAG.md!/build -> dag:DAG.md!/gen"; strings.Join(requires, ",") != want {
		t.Fatalf("requires edges: %v", requires)
	}
	if len(g.Nodes) != 4 || g.Nodes[0].Attrs["default"] != "build" || g.Nodes[0].Kind != "file" {
		t.Fatalf("nodes: %+v", g.Nodes)
	}
	for _, n := range g.Nodes {
		if n.Name == "gen" && (n.Kind != "target" || n.Type != "bash" || n.Attrs["sources"] == nil) {
			t.Fatalf("gen node: %+v", n)
		}
	}
}
