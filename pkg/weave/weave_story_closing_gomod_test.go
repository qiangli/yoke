package weave

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Inside a go.work, sibling pins are go.mod versions: a placeholder pin cannot
// be compared with the sibling's HEAD, so it blocks closing like a stale one.
func TestStalePinsReadsGoWorkPins(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	for rel, data := range map[string]string{
		"go.work":  "go 1.24\n\nuse (\n\t./a\n\t./b\n)\n",
		"a/go.mod": "module example.com/a\n\ngo 1.24\n\nrequire example.com/b v0.0.0\n",
		"b/go.mod": "module example.com/b\n\ngo 1.24\n",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := stalePins(filepath.Join(root, "a")); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("stalePins = %v, want [b]", got)
	}
	if got := stalePins(filepath.Join(root, "b")); got != nil {
		t.Fatalf("b has no sibling pins: %v", got)
	}
}
