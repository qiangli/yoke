package lenscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens"
)

func streamFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"top/sub/deep.txt", "top/a.txt", "top/b%40c.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(p, "%40", "@")))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Pull and push are the same bytes: for every view and depth,
// `lens -t VIEW dir:LOC` equals `lens -t graph dir:LOC | lens -t VIEW`, and
// the producer itself is byte-stable across runs of the same snapshot.
func TestPullPushIdentity(t *testing.T) {
	dir := streamFixture(t)
	for _, depth := range []string{"", "--depth=0", "--depth=1"} {
		args := func(to string, rest ...string) []string {
			a := []string{"-t", to}
			if depth != "" {
				a = append(a, depth)
			}
			return append(a, rest...)
		}
		produced, errb, code := runTool(t, dir, "", args("graph", "dir:top")...)
		if code != 0 || errb != "" {
			t.Fatalf("%s produce: %d %q", depth, code, errb)
		}
		if again, _, _ := runTool(t, dir, "", args("graph", "dir:top")...); again != produced {
			t.Fatalf("%s: producer is not deterministic", depth)
		}
		if !strings.HasPrefix(produced, `{"t":"header","schema_version":"bashy-graph-v1","source":"top",`) {
			t.Fatalf("%s: not framed:\n%s", depth, produced)
		}
		for _, view := range lens.GraphViews {
			pulled, errb, code := runTool(t, dir, "", args(view, "dir:top")...)
			if code != 0 || errb != "" {
				t.Fatalf("%s %s pull: %d %q", depth, view, code, errb)
			}
			pushed, errb, code := runTool(t, dir, produced, args(view)...)
			if code != 0 || errb != "" {
				t.Fatalf("%s %s push: %d %q", depth, view, code, errb)
			}
			if pulled != pushed {
				t.Errorf("%s %s: pull and push differ:\n%s\n---\n%s", depth, view, pulled, pushed)
			}
		}
	}
	tree, _, _ := runTool(t, dir, "", "-t", "tree", "dir:top")
	want := "top\n├── a.txt\n├── b@c.txt\n└── sub (1)\n    └── deep.txt\n"
	if tree != want {
		t.Fatalf("tree view:\n%s\nwant:\n%s", tree, want)
	}
	if shallow, _, _ := runTool(t, dir, "", "-t", "paths", "--depth=1", "dir:top"); strings.Contains(shallow, "deep") || !strings.Contains(shallow, "top/b%40c.txt\n") {
		t.Fatalf("--depth=1 paths (refs stay escaped locators):\n%s", shallow)
	}
}

// lens recognizes the stream by its declared header (no -f) and nothing else:
// a filter that drops the header needs `-f graph`.
func TestStreamRecognitionAndFilteredInput(t *testing.T) {
	dir := streamFixture(t)
	produced, _, _ := runTool(t, dir, "", "-t", "graph", "dir:top")
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(produced), 0o644); err != nil {
		t.Fatal(err)
	}
	// The header outranks the .jsonl extension.
	if out, errb, code := runTool(t, dir, "", "-t", "mermaid", "s.jsonl"); code != 0 || !strings.HasPrefix(out, "graph TD\n") {
		t.Fatalf("file with header: %d %q %q", code, out, errb)
	}
	var edgesOnly strings.Builder // jq -c 'select(.t=="edge")'
	for _, l := range strings.SplitAfter(produced, "\n") {
		if strings.HasPrefix(l, `{"t":"edge"`) {
			edgesOnly.WriteString(l)
		}
	}
	_, errb, code := runTool(t, dir, edgesOnly.String(), "-t", "dot")
	if code != lens.ExitUsage || !strings.Contains(errb, "-f graph") {
		t.Fatalf("headerless input must ask for -f graph: %d %q", code, errb)
	}
	out, errb, code := runTool(t, dir, edgesOnly.String(), "-f", "graph", "-t", "dot")
	if code != 0 || !strings.Contains(out, `"top" -> "top/sub" [label="contains"];`) {
		t.Fatalf("declared headerless input: %d %q %q", code, out, errb)
	}
	// Explicitly declaring another schema still wins over sniffing.
	if out, _, code := runTool(t, dir, edgesOnly.String(), "-f", "jsonl", "-t", "json"); code != 0 || !strings.HasPrefix(out, "[") {
		t.Fatalf("-f jsonl on records: %d %q", code, out)
	}
}

func TestLocatorOperands(t *testing.T) {
	dir := streamFixture(t)
	if _, errb, code := runTool(t, dir, "", "-t", "tree", "zip:x.zip"); code != 2 || !strings.Contains(errb, `provider "zip" is not registered`) {
		t.Fatalf("unregistered provider: %d %q", code, errb)
	}
	if _, errb, code := runTool(t, dir, "", "-f", "json", "-t", "tree", "dir:top"); code != 2 || !strings.Contains(errb, "-f does not apply") {
		t.Fatalf("-f with locator: %d %q", code, errb)
	}
	// A bare path is still a FILE (S3): no provider walk.
	if err := os.WriteFile(filepath.Join(dir, "in.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, code := runTool(t, dir, "", "-t", "yaml", "in.json"); code != 0 || out != "a: 1\n" {
		t.Fatalf("bare path: %d %q", code, out)
	}
}
