package lenscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gfygraph "github.com/qiangli/gfy/pkg/graph"
	"github.com/qiangli/yoke/pkg/codegraph"
)

func TestDocumentAndASTProvidersThroughLens(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"data.json": `{"items":[{"name":"first"}]}`,
		"data.yaml": "items:\n  - name: first\n",
		"sample.go": "package sample\nfunc Example() {}\n",
		"notes.md":  "# Plan\n## Next step\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ locator, leaf string }{
		{"json:data.json", "name"},
		{"yaml:data.yaml", "name"},
		{"ast:sample.go", "Example"},
		{"md:notes.md", "next-step"},
	} {
		out, errb, code := runTool(t, root, "", "-t", "paths", tc.locator)
		if code != 0 || errb != "" {
			t.Fatalf("%s: code=%d stderr=%q", tc.locator, code, errb)
		}
		if !strings.Contains(out, tc.leaf) {
			t.Fatalf("%s paths omit %q: %s", tc.locator, tc.leaf, out)
		}
	}
}

func TestGraphProviderThroughLens(t *testing.T) {
	root := t.TempDir()
	g := gfygraph.New(true)
	g.AddNode("n1", map[string]any{
		"label": "Example", "file_type": "code",
		"source_file": "sample.go", "source_location": "3",
	})
	g.AddNode("n2", map[string]any{
		"label": "Helper", "file_type": "code",
		"source_file": "sample.go", "source_location": "9",
	})
	g.AddEdge("n1", "n2", map[string]any{"relation": "calls", "confidence": "EXTRACTED"})
	if err := (&codegraph.GraphContext{Graph: g}).Save(codegraph.CachePath(root)); err != nil {
		t.Fatal(err)
	}
	out, errb, code := runTool(t, root, "", "-t", "paths", "graph:.")
	if code != 0 || errb != "" {
		t.Fatalf("paths: code=%d stderr=%q", code, errb)
	}
	for _, want := range []string{"graph:.", "graph:.!/sample.go", "graph:.!/sample.go!/Example"} {
		if !strings.Contains(out, want) {
			t.Fatalf("paths omit %q: %s", want, out)
		}
	}
	out, errb, code = runTool(t, root, "", "-t", "graph", "graph:.")
	if code != 0 || errb != "" {
		t.Fatalf("graph: code=%d stderr=%q", code, errb)
	}
	for _, want := range []string{"bashy-graph-v1", "graph:.!/sample.go!/Example", "calls"} {
		if !strings.Contains(out, want) {
			t.Fatalf("graph view omit %q: %s", want, out)
		}
	}
	// No cache is an explicit failure naming the fix, not an empty tree.
	if out, errb, code := runTool(t, t.TempDir(), "", "-t", "paths", "graph:."); code == 0 || !strings.Contains(errb, "graph build") {
		t.Fatalf("absence: code=%d out=%q err=%q", code, out, errb)
	}
}

func TestSystemProvidersRegistered(t *testing.T) {
	for _, scheme := range []string{"proc", "net", "pdf"} {
		if providers[scheme] == nil {
			t.Errorf("%s provider is not registered", scheme)
		}
	}
}
