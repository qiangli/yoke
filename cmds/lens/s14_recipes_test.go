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
