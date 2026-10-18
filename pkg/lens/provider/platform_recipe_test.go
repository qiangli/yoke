package provider

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
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

func TestK8sScopedRecipeContextArgvAndRefs(t *testing.T) {
	var got []string
	p, err := NewK8sScopedRecipe("kind-dev", func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`{"items":[{"metadata":{"name":"a","namespace":"default","uid":"u"},"status":{"phase":"Running"}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loc := ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "kind-dev"}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	want := []string{"kubectl", "--context", "kind-dev", "get", "pods", "--all-namespaces", "-o", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v", got)
	}
	if l, err := ref.ParseLocator(rows[0].Ref); err != nil || l.ScopeKind != "k8s" || l.ScopeValue != "kind-dev" {
		t.Fatalf("ref=%q err=%v", rows[0].Ref, err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "pods"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unscoped err=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "other"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("other-context err=%v", err)
	}
}

func TestCloudDropletsRecipeRowsAndReadOnlyCommand(t *testing.T) {
	var got []string
	p, err := NewCloudDropletsRecipe("team-ctx", func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`[{"id":222,"name":"bravo","status":"off"},{"id":111,"name":"alpha","status":"active"}]`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"doctl", "--context", "team-ctx", "compute", "droplet", "list", "--output", "json"}
	loc := ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "team-ctx"}
	root, err := p.Stat(context.Background(), loc)
	if err != nil || root.Children == nil || *root.Children != 2 {
		t.Fatalf("root=%+v err=%v", root, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v", got)
	}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 2 || rows[0].Name != "alpha" || rows[1].Name != "bravo" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Path != "droplets/111" || rows[0].Kind != "droplet" || rows[0].Type != "cloud/droplet" {
		t.Fatalf("row=%+v", rows[0])
	}
	if rows[0].Attrs["status"] != "active" || rows[1].Attrs["status"] != "off" {
		t.Fatalf("attrs=%v %v", rows[0].Attrs, rows[1].Attrs)
	}
	if !strings.Contains(rows[0].Ref, "@cloud:team-ctx") || !strings.HasPrefix(rows[0].Ref, "cloud:droplets/111@") {
		t.Fatalf("ref=%q", rows[0].Ref)
	}
	item, err := p.Stat(context.Background(), ref.Locator{Scheme: "cloud", Target: "droplets/111", ScopeKind: "cloud", ScopeValue: "team-ctx"})
	if err != nil || item.Ref != rows[0].Ref {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	if _, err := p.Open(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error=%v", err)
	}
	if _, err := p.Stat(context.Background(), ref.Locator{Scheme: "cloud", Target: "droplets/999", ScopeKind: "cloud", ScopeValue: "team-ctx"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "other"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("other-context err=%v", err)
	}
}

func TestCloudDropletsRejectInvalidSnapshots(t *testing.T) {
	for _, bad := range []string{`not json`, `null`, `[{"name":"no-id"}]`, `[{"id":"a/b"}]`, `[{"id":""}]`, `[{"id":1.5}]`, `[{"id":true}]`, `[{"id":0}]`, `[{"id":1}] true`} {
		if _, err := parseCloudDroplets([]byte(bad)); err == nil {
			t.Fatalf("accepted droplets %q", bad)
		}
	}
	if rows, err := parseCloudDroplets([]byte(`[]`)); err != nil || len(rows) != 0 {
		t.Fatalf("empty list=%v err=%v", rows, err)
	}
	if rows, err := parseCloudDroplets([]byte(`[{"id":9007199254740993,"name":"large"}]`)); err != nil || len(rows) != 1 || rows[0].id != "9007199254740993" {
		t.Fatalf("large exact id rows=%v err=%v", rows, err)
	}
	p, err := NewCloudDropletsRecipe("ctx", func(context.Context, string, ...string) ([]byte, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "ctx"}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("command error=%v", err)
	}
}

func TestCloudDropletsRefusesInvalidContext(t *testing.T) {
	for _, bad := range []string{"", "--token=x", "-x", "a b", "a=b", "a\nb", string(make([]byte, 254))} {
		if _, err := NewCloudDropletsRecipe(bad, func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner called")
			return nil, nil
		}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestK8sScopedRecipeRefusesInvalidContext(t *testing.T) {
	for _, bad := range []string{"", "--token=x", "-x", "a b", "a=b", "a\nb"} {
		if _, err := NewK8sScopedRecipe(bad, func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner called")
			return nil, nil
		}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
