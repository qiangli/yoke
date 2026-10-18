package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

const docTestJSON = `{"name":"yoke","count":3,"tags":["a","b"],"nested":{"flag":true,"nothing":null}}`

const docTestYAML = "name: yoke\ncount: 3\ntags:\n  - a\n  - b\nnested:\n  flag: true\n  nothing: null\n"

func docWriteTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDocumentStatRoot(t *testing.T) {
	jp := docWriteTemp(t, "doc.json", docTestJSON)
	yp := docWriteTemp(t, "doc.yaml", docTestYAML)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		provider *DocumentProvider
		target   string
		typ      string
	}{
		{"json", NewJSONProvider(), jp, "json"},
		{"yaml", NewYAMLProvider(), yp, "yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.provider.Stat(ctx, ref.Locator{Scheme: tc.typ, Target: tc.target})
			if err != nil {
				t.Fatal(err)
			}
			if n.Kind != "object" || n.Type != tc.typ {
				t.Fatalf("root kind/type = %q/%q, want object/%s", n.Kind, n.Type, tc.typ)
			}
			if n.Children == nil || *n.Children != 4 {
				t.Fatalf("root children = %v, want 4", n.Children)
			}
			if n.SchemaVersion != NodeSchema {
				t.Fatalf("schema = %q, want %q", n.SchemaVersion, NodeSchema)
			}
		})
	}
}

func TestDocumentStatLeafAttrs(t *testing.T) {
	jp := docWriteTemp(t, "doc.json", docTestJSON)
	ctx := context.Background()
	p := NewJSONProvider()

	n, err := p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp, Inner: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "string" || n.Attrs["value"] != "yoke" {
		t.Fatalf("leaf = kind %q value %v, want string/yoke", n.Kind, n.Attrs["value"])
	}
	if n.Path != "/name" || n.Name != "name" {
		t.Fatalf("leaf path/name = %q/%q, want /name/name", n.Path, n.Name)
	}

	n, err = p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp, Inner: []string{"nested", "flag"}})
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "bool" || n.Attrs["value"] != true {
		t.Fatalf("leaf = kind %q value %v, want bool/true", n.Kind, n.Attrs["value"])
	}
}

func TestDocumentListAndEdges(t *testing.T) {
	yp := docWriteTemp(t, "doc.yaml", docTestYAML)
	ctx := context.Background()
	p := NewYAMLProvider()
	root := ref.Locator{Scheme: "yaml", Target: yp}

	nodes, err := p.List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("list len = %d, want 4", len(nodes))
	}
	kinds := map[string]string{}
	for _, n := range nodes {
		kinds[n.Name] = n.Kind
	}
	if kinds["tags"] != "array" || kinds["count"] != "number" || kinds["nested"] != "object" {
		t.Fatalf("child kinds = %v", kinds)
	}

	arr, err := p.List(ctx, ref.Locator{Scheme: "yaml", Target: yp, Inner: []string{"tags"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 || arr[0].Attrs["value"] != "a" || arr[1].Name != "1" {
		t.Fatalf("array children = %+v", arr)
	}

	edges, err := p.Edges(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 4 {
		t.Fatalf("edges len = %d, want 4", len(edges))
	}
	for _, e := range edges {
		if e.Rel != "contains" || e.SchemaVersion != EdgeSchema {
			t.Fatalf("edge = %+v, want contains/%s", e, EdgeSchema)
		}
	}

	scalar, err := p.List(ctx, ref.Locator{Scheme: "yaml", Target: yp, Inner: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scalar) != 0 {
		t.Fatalf("scalar list len = %d, want 0", len(scalar))
	}
	scalarEdges, err := p.Edges(ctx, ref.Locator{Scheme: "yaml", Target: yp, Inner: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scalarEdges) != 0 {
		t.Fatalf("scalar edges len = %d, want 0", len(scalarEdges))
	}
}

func TestDocumentErrors(t *testing.T) {
	ctx := context.Background()
	p := NewJSONProvider()
	jp := docWriteTemp(t, "doc.json", docTestJSON)

	if _, err := p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp, Inner: []string{"missing"}}); err == nil {
		t.Fatal("missing key: want error")
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp, Inner: []string{"tags", "9"}}); err == nil {
		t.Fatal("bad index: want error")
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp, Inner: []string{"name", "deep"}}); err == nil {
		t.Fatal("descend into scalar: want error")
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "json", Target: jp + ".absent"}); err == nil {
		t.Fatal("missing file: want error")
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "yaml", Target: jp}); err == nil {
		t.Fatal("wrong scheme: want error")
	}
}

func TestDocumentOpenReadOnly(t *testing.T) {
	jp := docWriteTemp(t, "doc.json", docTestJSON)
	ctx := context.Background()
	p := NewJSONProvider()

	rc, err := p.Open(ctx, ref.Locator{Scheme: "json", Target: jp})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	rc.Close()
}
