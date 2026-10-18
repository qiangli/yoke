package lens

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// A stream view renders exactly what S11's live-walk view renders for the
// same locator and depth (json differs by design: a stream may have several
// roots, so it is always an array).
func TestStreamViewsMatchS11Views(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"top/sub/deep.txt", "top/a.txt", "top/z.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := provider.NewDir(root, false)
	loc := ref.Locator{Scheme: "dir", Target: "top"}
	ctx := context.Background()
	for _, depth := range []int{-1, 0, 1, 2} {
		g, err := PullGraph(ctx, p, loc, depth)
		if err != nil {
			t.Fatal(err)
		}
		var framed bytes.Buffer
		if err := stream.Encode(&framed, g, "dir", PullQuery(depth)); err != nil {
			t.Fatal(err)
		}
		decoded, err := stream.Decode(&framed, stream.DecodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, view := range []string{"listing", "paths", "outline", "tree", "edges", "dot", "mermaid"} {
			var live, fromStream bytes.Buffer
			if err := Render(ctx, &live, p, loc, view, depth, false); err != nil {
				t.Fatal(err)
			}
			if err := RenderGraph(&fromStream, decoded, view, ViewOptions{}); err != nil {
				t.Fatal(err)
			}
			if live.String() != fromStream.String() {
				t.Errorf("depth %d %s:\nS11:\n%s\nstream:\n%s", depth, view, live.String(), fromStream.String())
			}
		}
	}
}
