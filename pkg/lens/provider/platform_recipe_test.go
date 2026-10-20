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
	for _, bad := range []string{
		`not json`,
		`[{"name":"no-id"}]`,
		`[{"id":null,"name":"nil-id"}]`,
		`[{"id":"a/b"}]`,
		`[{"id":""}]`,
		`[{"id":7.5}]`,
		`[{"id":-1}]`,
		`[{"id":"-1"}]`,
		`[{"id":true}]`,
		`[{"id":[7]}]`,
		`[{"id":18446744073709551616}]`,
		`[{"id":"18446744073709551616"}]`,
		`[{"id":"0x10"}]`,
	} {
		if _, err := parseCloudDroplets([]byte(bad)); err == nil {
			t.Fatalf("accepted droplets %q", bad)
		}
	}
	// A top-level null is not an empty list.
	if _, err := parseCloudDroplets([]byte(`null`)); err == nil {
		t.Fatal("accepted top-level null droplet list")
	}
	// Trailing JSON after a valid array is rejected, not ignored.
	for _, bad := range []string{
		`[] {}`,
		`[{"id":1,"name":"a"}] [{"id":2,"name":"b"}]`,
		`[{"id":1,"name":"a"}] null`,
		`[] trailing`,
	} {
		if _, err := parseCloudDroplets([]byte(bad)); err == nil {
			t.Fatalf("accepted droplets with trailing data %q", bad)
		}
	}
	// Large ids keep full precision: 2^53+1 must not collapse to 2^53.
	rows, err := parseCloudDroplets([]byte(`[{"id":9007199254740993,"name":"big"}]`))
	if err != nil || len(rows) != 1 || rows[0].id != "9007199254740993" {
		t.Fatalf("large id rows=%+v err=%v", rows, err)
	}
	// Quoted digits are accepted in canonical form.
	rows, err = parseCloudDroplets([]byte(`[{"id":"42","name":"quoted"}]`))
	if err != nil || len(rows) != 1 || rows[0].id != "42" {
		t.Fatalf("quoted id rows=%+v err=%v", rows, err)
	}
	if rows, err := parseCloudDroplets([]byte(`[]`)); err != nil || len(rows) != 0 {
		t.Fatalf("empty list=%v err=%v", rows, err)
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

func TestK8sNodesRecipeRowsAndReadOnlyCommand(t *testing.T) {
	p := NewK8sNodesRecipe(func(_ context.Context, c string, a ...string) ([]byte, error) {
		if c != "kubectl" || !reflect.DeepEqual(a, []string{"get", "nodes", "-o", "json"}) {
			t.Fatalf("command=%s args=%v", c, a)
		}
		return []byte(`{"items":[
			{"metadata":{"name":"node-b","uid":"ub","labels":{"node-role.kubernetes.io/worker":""}},"status":{"conditions":[{"type":"Ready","status":"False"}],"nodeInfo":{"kubeletVersion":"v1.29.2"}}},
			{"metadata":{"name":"node-a","uid":"ua","labels":{"node-role.kubernetes.io/control-plane":"","kubernetes.io/role":"master"}},"status":{"conditions":[{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.30.0"}}},
			{"metadata":{"name":"node-c","uid":"uc"},"status":{}}
		]}`), nil
	})
	loc := ref.Locator{Scheme: "k8s", Target: "nodes"}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Path != "nodes/node-a" || rows[1].Path != "nodes/node-b" || rows[2].Path != "nodes/node-c" {
		t.Fatalf("unsorted rows=%+v", rows)
	}
	if rows[0].Kind != "node" || rows[0].Type != "k8s/node" {
		t.Fatalf("row=%+v", rows[0])
	}
	if rows[0].Attrs["ready"] != "True" || rows[0].Attrs["kubeletVersion"] != "v1.30.0" || rows[0].Attrs["roles"] != "control-plane,master" || rows[0].Attrs["uid"] != "ua" {
		t.Fatalf("attrs=%v", rows[0].Attrs)
	}
	if rows[1].Attrs["ready"] != "False" || rows[1].Attrs["roles"] != "worker" {
		t.Fatalf("attrs=%v", rows[1].Attrs)
	}
	// A node without conditions or labels still lists with Unknown readiness.
	if rows[2].Attrs["ready"] != "Unknown" || rows[2].Attrs["roles"] != "" {
		t.Fatalf("attrs=%v", rows[2].Attrs)
	}
	item, err := p.Stat(context.Background(), ref.Locator{Scheme: "k8s", Target: "nodes/node-a"})
	if err != nil || item.Ref != rows[0].Ref {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	if _, err := p.Stat(context.Background(), ref.Locator{Scheme: "k8s", Target: "nodes/missing"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v", err)
	}
}

func TestK8sNodesRejectInvalidSnapshots(t *testing.T) {
	for _, bad := range []string{`not json`, `{"items":[{"metadata":{}}]}`, `{"items":[{"metadata":{"name":"a/b"}}]}`} {
		if _, err := parseK8sNodes([]byte(bad)); err == nil {
			t.Fatalf("accepted nodes %q", bad)
		}
	}
	if rows, err := parseK8sNodes([]byte(`{"items":[]}`)); err != nil || len(rows) != 0 {
		t.Fatalf("empty list=%v err=%v", rows, err)
	}
}

func TestK8sNodesScopedRecipeContextArgvAndRefs(t *testing.T) {
	var got []string
	p, err := NewK8sNodesScopedRecipe("kind-dev", func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`{"items":[{"metadata":{"name":"n1","uid":"u1","labels":{"node-role.kubernetes.io/control-plane":""}},"status":{"conditions":[{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.30.0"}}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loc := ref.Locator{Scheme: "k8s", Target: "nodes", ScopeKind: "k8s", ScopeValue: "kind-dev"}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 || rows[0].Name != "n1" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	want := []string{"kubectl", "--context", "kind-dev", "get", "nodes", "-o", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v", got)
	}
	if l, err := ref.ParseLocator(rows[0].Ref); err != nil || l.ScopeKind != "k8s" || l.ScopeValue != "kind-dev" {
		t.Fatalf("ref=%q err=%v", rows[0].Ref, err)
	}
	if _, err := p.Open(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "nodes"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unscoped err=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "nodes", ScopeKind: "k8s", ScopeValue: "other"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("other-context err=%v", err)
	}
	for _, bad := range []string{"", "--token=x", "-x", "a b", "a=b", "a\nb"} {
		if _, err := NewK8sNodesScopedRecipe(bad, func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner called")
			return nil, nil
		}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// TestTierSandboxRecipeArgvReadOnly pins the sandbox tier transport: the
// outpost SSH target travels as literal argv, only the read-only podman ps
// verb is ever issued, refs keep the peer scope, Open has no write-capable
// stream, and an invalid scope or target is refused before execution.
func TestTierSandboxRecipeArgvReadOnly(t *testing.T) {
	var got []string
	p, err := NewOCISandboxRecipe("novidesign", "dogtest", func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`[{"Id":"c1","Names":["web"],"Image":"img:web","State":"running"}]`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loc := ref.Locator{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "novidesign"}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 || rows[0].Name != "web" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if want := []string{"outpost", "ssh", "exec", "dogtest", "--", "podman", "ps", "--all", "--format", "json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v want %v", got, want)
	}
	sep := -1
	for i, tok := range got {
		if tok == "--" {
			sep = i
		}
	}
	tail := got[sep+1:]
	if len(tail) < 2 || tail[0] != "podman" || tail[1] != "ps" {
		t.Fatalf("podman argv tail=%v, want podman ps ...", tail)
	}
	for _, tok := range tail {
		for _, w := range []string{"create", "run", "start", "stop", "rm", "rmi", "pull", "push", "build", "kill", "restart", "pause", "unpause", "commit", "rename", "put"} {
			if tok == w {
				t.Fatalf("argv %v contains write verb %q", got, w)
			}
		}
	}
	if l, err := ref.ParseLocator(rows[0].Ref); err != nil || l.ScopeKind != "sandbox" || l.ScopeValue != "novidesign" {
		t.Fatalf("ref=%q err=%v", rows[0].Ref, err)
	}
	if _, err := p.Open(context.Background(), loc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open error=%v, want ErrUnsupported", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "containers"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unscoped err=%v", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "other"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("other-scope err=%v", err)
	}
	for _, bad := range [][2]string{{"", "dogtest"}, {"novidesign", ""}, {"-x", "dogtest"}, {"novidesign", "a b"}, {"a=b", "dogtest"}} {
		if _, err := NewOCISandboxRecipe(bad[0], bad[1], func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner called")
			return nil, nil
		}); err == nil {
			t.Fatalf("accepted scope=%q target=%q", bad[0], bad[1])
		}
	}
}
