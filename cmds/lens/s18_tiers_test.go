package lenscmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// TestTierVerbosePrintsTierAndTransport runs full locator pulls with injected
// runners (no live cluster or cloud): --verbose names the resolved tier and
// transport, and the listing rows carry the explicit scope.
func TestTierVerbosePrintsTierAndTransport(t *testing.T) {
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		return []byte(`{"items":[{"metadata":{"name":"a","namespace":"default","uid":"u"},"status":{"phase":"Running"}}]}`), nil
	}
	cloudRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		return []byte(`[{"id":7,"name":"web","status":"active"}]`), nil
	}
	defer func() { k8sRunner, cloudRunner = nil, nil }()
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "do-secret-token")
	t.Setenv("DIGITALOCEAN_CONTEXT", "ambient-should-be-ignored")
	t.Setenv("KUBECONFIG", "/tmp/secret-kubeconfig")

	for _, c := range []struct {
		operand string
		line    string
	}{
		{"k8s:pods@k8s:kind-dev", "lens: tier k8s transport kubectl scope k8s:kind-dev\n"},
		{"cloud:droplets@cloud:team-ctx", "lens: tier cloud transport doctl scope cloud:team-ctx\n"},
	} {
		out, errb, code := runTool(t, t.TempDir(), "", "-t", "graph", "--verbose", c.operand)
		if code != 0 {
			t.Fatalf("%s: code=%d out=%q err=%q", c.operand, code, out, errb)
		}
		if !strings.HasPrefix(errb, c.line) {
			t.Fatalf("%s: stderr=%q want prefix %q", c.operand, errb, c.line)
		}
		for _, secret := range []string{"do-secret-token", "ambient-should-be-ignored", "secret-kubeconfig"} {
			if strings.Contains(errb, secret) || strings.Contains(out, secret) {
				t.Fatalf("%s: output leaks %q", c.operand, secret)
			}
		}
	}
}

// TestTierExplicitScopeFailClosed pins no-ambient-fallback at the CLI layer:
// unscoped or cross-scoped k8s/cloud locators fail before any command runs,
// and an invalid context is refused without execution.
func TestTierExplicitScopeFailClosed(t *testing.T) {
	for _, loc := range []ref.Locator{
		{Scheme: "k8s", Target: "pods"},
		{Scheme: "cloud", Target: "droplets"},
		{Scheme: "k8s", Target: "pods", ScopeKind: "cloud", ScopeValue: "x"},
		{Scheme: "cloud", Target: "droplets", ScopeKind: "k8s", ScopeValue: "x"},
		{Scheme: "oci", Target: "containers", ScopeKind: "k8s", ScopeValue: "x"},
		{Scheme: "dir", Target: ".", ScopeKind: "cloud", ScopeValue: "x"},
	} {
		if _, err := scopedProvider(nil, loc); err == nil {
			t.Errorf("%v: scoped pull accepted", loc)
		}
	}
	if _, err := providers["k8s"](nil, ref.Locator{Scheme: "k8s", Target: "pods"}); err == nil {
		t.Error("unscoped k8s accepted by provider factory")
	}
	if _, err := providers["cloud"](nil, ref.Locator{Scheme: "cloud", Target: "droplets"}); err == nil {
		t.Error("unscoped cloud accepted by provider factory")
	}
	ran := false
	k8sRunner = func(context.Context, string, ...string) ([]byte, error) {
		ran = true
		return nil, nil
	}
	defer func() { k8sRunner = nil }()
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "-bad"}); err == nil || ran {
		t.Errorf("bad k8s context accepted or executed (ran=%v err=%v)", ran, err)
	}
	ranCloud := false
	cloudRunner = func(context.Context, string, ...string) ([]byte, error) {
		ranCloud = true
		return nil, nil
	}
	defer func() { cloudRunner = nil }()
	if _, err := scopedProvider(nil, ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "a b"}); err == nil || ranCloud {
		t.Errorf("bad cloud context accepted or executed (ran=%v err=%v)", ranCloud, err)
	}
}

// TestTierContextArgvReadOnly pins the read-only listing contract: the
// explicit context travels as a literal --context flag, the verb is a listing
// (get/list), no write verb ever appears, and the recipe exposes no
// write-capable stream.
func TestTierContextArgvReadOnly(t *testing.T) {
	var k8sArgv, cloudArgv []string
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		k8sArgv = append([]string{c}, a...)
		return []byte(`{"items":[]}`), nil
	}
	cloudRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		cloudArgv = append([]string{c}, a...)
		return []byte(`[]`), nil
	}
	defer func() { k8sRunner, cloudRunner = nil, nil }()

	k8sLoc := ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "prod-ctx"}
	cloudLoc := ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "prod-ctx"}
	k8sP, err := scopedProvider(nil, k8sLoc)
	if err != nil {
		t.Fatal(err)
	}
	cloudP, err := scopedProvider(nil, cloudLoc)
	if err != nil {
		t.Fatal(err)
	}
	// The cloud factory and the scoped router must serve the same recipe:
	// no duplicate provider registry.
	direct, err := providers["cloud"](nil, cloudLoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p   provider.Provider
		loc ref.Locator
	}{
		{k8sP, k8sLoc},
		{cloudP, cloudLoc},
		{direct, cloudLoc},
	} {
		if _, err := c.p.List(context.Background(), c.loc); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"kubectl", "--context", "prod-ctx", "get", "pods", "--all-namespaces", "-o", "json"}; !reflect.DeepEqual(k8sArgv, want) {
		t.Errorf("kubectl argv=%v want %v", k8sArgv, want)
	}
	if want := []string{"doctl", "--context", "prod-ctx", "compute", "droplet", "list", "--output", "json"}; !reflect.DeepEqual(cloudArgv, want) {
		t.Errorf("doctl argv=%v want %v", cloudArgv, want)
	}
	writeVerbs := []string{"create", "apply", "delete", "patch", "replace", "edit", "write", "update", "scale", "rollout", "exec", "run", "destroy"}
	for _, argv := range [][]string{k8sArgv, cloudArgv} {
		for _, tok := range argv {
			for _, w := range writeVerbs {
				if tok == w {
					t.Errorf("argv %v contains write verb %q", argv, w)
				}
			}
		}
	}
	for _, c := range []struct {
		p   provider.Provider
		loc ref.Locator
	}{
		{k8sP, k8sLoc},
		{cloudP, cloudLoc},
		{direct, cloudLoc},
	} {
		if _, err := c.p.Open(context.Background(), c.loc); !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("%v: Open error=%v, want ErrUnsupported (no write-capable stream)", c.loc, err)
		}
	}
}
