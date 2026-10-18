package lens

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestViewsShareBoundedWalk(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "deep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	p := provider.NewDir(root, false)
	loc := ref.Locator{Scheme: "dir", Target: "."}
	for _, view := range []string{"listing", "tree", "outline", "paths", "json"} {
		var b bytes.Buffer
		if err := Render(context.Background(), &b, p, loc, view, 1, false); err != nil {
			t.Fatalf("%s: %v", view, err)
		}
		if strings.Contains(b.String(), "deep") {
			t.Errorf("%s exceeded depth: %s", view, b.String())
		}
	}
	var edges bytes.Buffer
	if err := Render(context.Background(), &edges, p, loc, "edges", 1, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(edges.String(), "deep") || !strings.Contains(edges.String(), `"schema_version":"bashy-edge-v1"`) {
		t.Fatalf("unbounded or invalid edges: %s", edges.String())
	}
}
