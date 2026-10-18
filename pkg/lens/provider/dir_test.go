package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestDirRows(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	d := NewDir(root, false)
	loc := ref.Locator{Scheme: "dir", Target: "."}
	rows, err := d.List(context.Background(), loc)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Children == nil || *rows[0].Children != 1 {
		t.Fatalf("children=%v", rows[0].Children)
	}
	b, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != NodeSchema || m["attrs"] == nil {
		t.Fatalf("row=%s", b)
	}
}
