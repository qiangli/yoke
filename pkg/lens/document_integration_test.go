package lens

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestWalkDocumentContainers(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		body   string
	}{
		{"json", `{"items":[{"name":"first"}]}`},
		{"yaml", "items:\n  - name: first\n"},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "document")
			if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &provider.DocumentProvider{Scheme: tc.scheme}
			tree, err := Walk(context.Background(), p, ref.Locator{Scheme: tc.scheme, Target: file}, -1)
			if err != nil {
				t.Fatal(err)
			}
			if len(tree.Children) != 1 || len(tree.Children[0].Children) != 1 ||
				len(tree.Children[0].Children[0].Children) != 1 {
				t.Fatalf("nested document was not traversed: %+v", tree)
			}
			if got := tree.Children[0].Children[0].Children[0].Node.Attrs["value"]; got != "first" {
				t.Fatalf("leaf value = %v, want first", got)
			}
		})
	}
}
