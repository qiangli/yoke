package lens

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestWalkASTSymbols(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\nfunc Example() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := Walk(context.Background(), provider.NewAST(root), ref.Locator{Scheme: "ast", Target: "sample.go"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) == 0 {
		t.Fatalf("ast file did not expand into symbols: %+v", tree.Node)
	}
	found := false
	for _, child := range tree.Children {
		if child.Node.Name == "Example" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Example symbol missing: %+v", tree.Children)
	}
}
