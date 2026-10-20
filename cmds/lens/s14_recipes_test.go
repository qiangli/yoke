package lenscmd

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestPlatformRecipesRegistered(t *testing.T) {
	for _, scheme := range []string{"oci", "k8s", "cloud"} {
		if providers[scheme] == nil {
			t.Errorf("%s provider is not registered", scheme)
		}
	}
}

func TestCloudDropletsPullArgvAndFailClosed(t *testing.T) {
	var got []string
	cloudRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`[{"id":7,"name":"web","status":"active"}]`), nil
	}
	defer func() { cloudRunner = nil }()
	t.Setenv("DIGITALOCEAN_CONTEXT", "ambient-should-be-ignored")
	loc := ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "prod-ctx"}
	p, err := providers["cloud"](nil, loc)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Ref, "@cloud:prod-ctx") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if !reflect.DeepEqual(got, []string{"doctl", "--context", "prod-ctx", "compute", "droplet", "list", "--output", "json"}) {
		t.Fatalf("argv=%v", got)
	}
	if _, err := providers["cloud"](nil, ref.Locator{Scheme: "cloud", Target: "droplets"}); err == nil {
		t.Fatal("unscoped cloud accepted")
	}
	if _, err := providers["cloud"](nil, ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "k8s", ScopeValue: "x"}); err == nil {
		t.Fatal("cloud@k8s accepted")
	}
	if _, err := providers["cloud"](nil, ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "-bad"}); err == nil {
		t.Fatal("bad context accepted")
	}
}

func TestK8sScopedPullArgvAndFailClosed(t *testing.T) {
	var got []string
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`{"items":[{"metadata":{"name":"a","namespace":"default"},"status":{"phase":"Running"}}]}`), nil
	}
	defer func() { k8sRunner = nil }()
	loc := ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "prod-ctx"}
	p, err := scopedProvider(nil, loc)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Ref, "@k8s:prod-ctx") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if !reflect.DeepEqual(got, []string{"kubectl", "--context", "prod-ctx", "get", "pods", "--all-namespaces", "-o", "json"}) {
		t.Fatalf("argv=%v", got)
	}
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "oci", Target: "containers", ScopeKind: "k8s", ScopeValue: "x"}); err == nil {
		t.Fatal("oci@k8s accepted")
	}
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "-bad"}); err == nil {
		t.Fatal("bad context accepted")
	}
}

func TestK8sNodesScopedPullArgvAndFailClosed(t *testing.T) {
	var got []string
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`{"items":[{"metadata":{"name":"n1","uid":"u1","labels":{"node-role.kubernetes.io/control-plane":""}},"status":{"conditions":[{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.30.0"}}}]}`), nil
	}
	defer func() { k8sRunner = nil }()
	loc := ref.Locator{Scheme: "k8s", Target: "nodes", ScopeKind: "k8s", ScopeValue: "prod-ctx"}
	p, err := scopedProvider(nil, loc)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.List(context.Background(), loc)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Ref, "@k8s:prod-ctx") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Attrs["ready"] != "True" || rows[0].Attrs["kubeletVersion"] != "v1.30.0" || rows[0].Attrs["roles"] != "control-plane" {
		t.Fatalf("attrs=%v", rows[0].Attrs)
	}
	if !reflect.DeepEqual(got, []string{"kubectl", "--context", "prod-ctx", "get", "nodes", "-o", "json"}) {
		t.Fatalf("argv=%v", got)
	}
	// The scoped router and the factory serve the same recipe: no duplicate
	// registry.
	direct, err := providers["k8s"](nil, loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.List(context.Background(), loc); err != nil {
		t.Fatal(err)
	}
	if _, err := providers["k8s"](nil, ref.Locator{Scheme: "k8s", Target: "nodes"}); err == nil {
		t.Fatal("unscoped k8s nodes accepted by provider factory")
	}
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "k8s", Target: "deployments", ScopeKind: "k8s", ScopeValue: "x"}); err == nil {
		t.Fatal("k8s deployments accepted")
	}
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "k8s", Target: "nodes", ScopeKind: "k8s", ScopeValue: "-bad"}); err == nil {
		t.Fatal("bad context accepted")
	}
	// The pods collection still routes to the pods recipe, not nodes: the
	// runner sees the pods verb.
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(`{"items":[]}`), nil
	}
	pods, err := providers["k8s"](nil, ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "prod-ctx"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pods.List(context.Background(), ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "prod-ctx"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"kubectl", "--context", "prod-ctx", "get", "pods", "--all-namespaces", "-o", "json"}) {
		t.Fatalf("argv=%v", got)
	}
}
