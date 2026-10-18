package provider

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestOCIRecipeRowsAndEdges(t *testing.T) {
	var command string
	var args []string
	p := NewOCIRecipe(func(_ context.Context, c string, a ...string) ([]byte, error) {
		command, args = c, a
		return []byte(`[{"Id":"b222","Names":["bravo"],"Image":"img:b","State":"running"},{"Id":"a111","Names":["alpha"],"Image":"img:a","State":"exited"}]`), nil
	})
	loc := ref.Locator{Scheme: "oci", Target: "containers"}
	root, err := p.Stat(context.Background(), loc)
	if err != nil || root.Children == nil || *root.Children != 2 {
		t.Fatalf("root=%+v err=%v", root, err)
	}
	if command != "podman" || !reflect.DeepEqual(args, []string{"ps", "--all", "--format", "json"}) {
		t.Fatalf("command=%s args=%v", command, args)
	}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 2 || rows[0].Name != "alpha" || rows[1].Name != "bravo" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	edges, err := p.Edges(context.Background(), loc)
	if err != nil || len(edges) != 2 || edges[0].Src != loc.String() || edges[0].Dst != rows[0].Ref {
		t.Fatalf("edges=%+v err=%v", edges, err)
	}
	item, err := p.Stat(context.Background(), ref.Locator{Scheme: "oci", Target: "containers/a111"})
	if err != nil || item.Ref != rows[0].Ref {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	if _, err := p.Open(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error=%v", err)
	}
	if _, err := p.Stat(context.Background(), ref.Locator{Scheme: "oci", Target: "containers/missing"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v", err)
	}
}

func TestK8sRecipePodsAndReadOnlyCommand(t *testing.T) {
	p := NewK8sRecipe(func(_ context.Context, c string, a ...string) ([]byte, error) {
		if c != "kubectl" || !reflect.DeepEqual(a, []string{"get", "pods", "--all-namespaces", "-o", "json"}) {
			t.Fatalf("command=%s args=%v", c, a)
		}
		return []byte(`{"items":[{"metadata":{"name":"second","namespace":"team","uid":"u2"},"status":{"phase":"Pending"}},{"metadata":{"name":"first","namespace":"default","uid":"u1"},"status":{"phase":"Running"}}]}`), nil
	})
	loc := ref.Locator{Scheme: "k8s", Target: "pods"}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 2 || rows[0].Path != "pods/default/first" || rows[1].Path != "pods/team/second" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Attrs["phase"] != "Running" || rows[0].Attrs["uid"] != "u1" {
		t.Fatalf("attrs=%v", rows[0].Attrs)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "pods/default/first"}); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformRecipesRejectInvalidSnapshots(t *testing.T) {
	for _, bad := range []string{`not json`, `[{"Names":["no-id"]}]`} {
		if _, err := parseOCI([]byte(bad)); err == nil {
			t.Fatalf("accepted OCI %q", bad)
		}
	}
	if _, err := parseK8sPods([]byte(`{"items":[{"metadata":{}}]}`)); err == nil {
		t.Fatal("accepted pod without name")
	}
	p := NewOCIRecipe(func(context.Context, string, ...string) ([]byte, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "containers"}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("command error=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "../containers"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid locator error=%v", err)
	}
	if rows, err := parseK8sPods([]byte(`{"items":[]}`)); err != nil || len(rows) != 0 {
		t.Fatalf("empty list=%v err=%v", rows, err)
	}
}
