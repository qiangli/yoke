package lenscmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	oldP, oldT, oldS := peerRunner, hostTargetsRunner, sandboxRunner
	peerRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"host":"novidesign","online":true}]`), nil
	}
	hostTargetsRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"name":"dogtest","host":"novidesign"}]`), nil
	}
	sandboxRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		return []byte(`[{"Id":"c1","Names":["web"],"Image":"img:web","State":"running"}]`), nil
	}
	defer func() { peerRunner, hostTargetsRunner, sandboxRunner = oldP, oldT, oldS }()
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "do-secret-token")
	t.Setenv("DIGITALOCEAN_CONTEXT", "ambient-should-be-ignored")
	t.Setenv("KUBECONFIG", "/tmp/secret-kubeconfig")

	for _, c := range []struct {
		operand string
		line    string
	}{
		{"k8s:pods@k8s:kind-dev", "lens: tier k8s transport kubectl scope k8s:kind-dev\n"},
		{"cloud:droplets@cloud:team-ctx", "lens: tier cloud transport doctl scope cloud:team-ctx\n"},
		{"oci:containers@sandbox:novidesign", "lens: tier sandbox transport podman scope sandbox:novidesign\n"},
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
		{Scheme: "dir", Target: "src", ScopeKind: "sandbox", ScopeValue: "x"},
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

// TestTierCommonOptionsUniform audits the S18 promise that one locator
// grammar reaches every tier with the same views and common options: -t
// listing/paths/edges, --depth, --null and --verbose behave identically for
// the workspace, sandbox, host, k8s and cloud scopes, every transport stays
// read-only, and nothing is written. All fixtures are isolated (a temp HOME
// weave layout plus canned peers, SSH targets and runners): no live cloud,
// cluster or peer is contacted, no credential is acquired, and the agent
// mailbox is never touched.
func TestTierCommonOptionsUniform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	wsSrc := filepath.Join(home, ".bashy", "weave", "repo-0000abcd", "workspaces", "issue-7", "src")
	if err := os.MkdirAll(wsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsSrc, "a.txt"), []byte("in workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	oldP, oldT, oldS, oldK, oldC, oldH := peerRunner, hostTargetsRunner, sandboxRunner, k8sRunner, cloudRunner, hostTransportImpl
	peerRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"host":"novidesign","online":true},{"host":"mac-1.example","alias":"mac","online":true}]`), nil
	}
	hostTargetsRunner = func(context.Context) ([]byte, error) {
		return []byte(`[{"name":"dogtest","host":"novidesign"},{"name":"mac-target","host":"mac-1.example"}]`), nil
	}
	fh := &fakeHost{
		tree: map[string][]hostEntry{
			".":   {{Name: "src", Dir: true}},
			"src": {{Name: "a.txt", Size: 5}},
		},
		files: map[string]string{"src/a.txt": "hello"},
	}
	hostTransportImpl = fh
	var k8sArgv, cloudArgv, sbArgv []string
	k8sRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		k8sArgv = append([]string{c}, a...)
		return []byte(`{"items":[{"metadata":{"name":"a","namespace":"default","uid":"u"},"status":{"phase":"Running"}}]}`), nil
	}
	cloudRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		cloudArgv = append([]string{c}, a...)
		return []byte(`[{"id":7,"name":"web","status":"active"}]`), nil
	}
	sandboxRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		sbArgv = append([]string{c}, a...)
		return []byte(`[{"Id":"c1","Names":["web"],"Image":"img:web","State":"running"}]`), nil
	}
	defer func() {
		peerRunner, hostTargetsRunner, sandboxRunner, k8sRunner, cloudRunner, hostTransportImpl = oldP, oldT, oldS, oldK, oldC, oldH
	}()
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "do-secret-token")
	t.Setenv("DIGITALOCEAN_CONTEXT", "ambient-should-be-ignored")
	t.Setenv("KUBECONFIG", "/tmp/secret-kubeconfig")

	tiers := []struct {
		operand string
		scope   string
		verbose string
		child   string
	}{
		{"dir:src@ws:issue-7", "@ws:issue-7", "lens: tier workspace transport local scope ws:issue-7\n", "a.txt"},
		{"oci:containers@sandbox:novidesign", "@sandbox:novidesign", "lens: tier sandbox transport podman scope sandbox:novidesign\n", "web"},
		{"dir:src@mac", "@mac", "lens: tier sphere transport outpost-sftp scope host:mac\n", "a.txt"},
		{"k8s:pods@k8s:kind-dev", "@k8s:kind-dev", "lens: tier k8s transport kubectl scope k8s:kind-dev\n", `"name":"a"`},
		{"cloud:droplets@cloud:team-ctx", "@cloud:team-ctx", "lens: tier cloud transport doctl scope cloud:team-ctx\n", "web"},
	}
	for _, tc := range tiers {
		// Every tier serves the same views with the explicit scope on rows.
		for _, view := range []string{"graph", "listing", "paths", "edges"} {
			out, errb, code := runTool(t, cwd, "", "-t", view, tc.operand)
			if code != 0 {
				t.Fatalf("%s %s: code=%d stderr=%q", tc.operand, view, code, errb)
			}
			if view != "edges" && !strings.Contains(out, tc.scope) {
				t.Fatalf("%s %s: rows lost the explicit scope:\n%s", tc.operand, view, out)
			}
		}
		// --null terminates the paths view on every tier.
		out, _, code := runTool(t, cwd, "", "-t", "paths", "--null", tc.operand)
		if code != 0 || !strings.Contains(out, "\x00") || !strings.Contains(out, tc.scope) {
			t.Fatalf("%s: --null paths view not NUL-terminated with scope: code=%d out=%q", tc.operand, code, out)
		}
		// --depth 0 shows the root only on every tier: same walk bound.
		out, _, code = runTool(t, cwd, "", "-t", "listing", "--depth=0", tc.operand)
		if code != 0 {
			t.Fatalf("%s: --depth=0 code=%d", tc.operand, code)
		}
		if strings.Contains(out, tc.child) {
			t.Fatalf("%s: --depth=0 leaked a child row:\n%s", tc.operand, out)
		}
		// --verbose names the resolved tier and transport without secrets.
		_, errb, code := runTool(t, cwd, "", "-t", "graph", "--verbose", tc.operand)
		if code != 0 {
			t.Fatalf("%s: --verbose code=%d stderr=%q", tc.operand, code, errb)
		}
		if !strings.HasPrefix(errb, tc.verbose) {
			t.Fatalf("%s: stderr=%q want prefix %q", tc.operand, errb, tc.verbose)
		}
		for _, secret := range []string{"do-secret-token", "ambient-should-be-ignored", "secret-kubeconfig"} {
			if strings.Contains(errb, secret) {
				t.Fatalf("%s: --verbose leaks %q", tc.operand, secret)
			}
		}
	}

	// No transport issued a write verb; the host transport saw only ls/get.
	writeVerbs := []string{"create", "apply", "delete", "patch", "replace", "edit", "write", "update", "scale", "rollout", "exec", "run", "destroy", "start", "stop", "rm", "rmi", "pull", "push", "build", "kill", "restart", "commit", "put"}
	for _, argv := range [][]string{k8sArgv, cloudArgv} {
		for _, tok := range argv {
			for _, w := range writeVerbs {
				if tok == w {
					t.Errorf("argv %v contains write verb %q", argv, w)
				}
			}
		}
	}
	sep := -1
	for i, tok := range sbArgv {
		if tok == "--" {
			sep = i
		}
	}
	if sep < 0 || sep+2 > len(sbArgv) || sbArgv[sep+1] != "podman" || sbArgv[sep+2] != "ps" {
		t.Fatalf("sandbox argv=%v, want outpost ssh exec <target> -- podman ps ...", sbArgv)
	}
	for _, tok := range sbArgv[sep+1:] {
		for _, w := range writeVerbs {
			if tok == w {
				t.Fatalf("sandbox argv %v contains write verb %q", sbArgv, w)
			}
		}
	}
	for _, c := range fh.calls {
		if !strings.HasPrefix(c, "ls mac-target ") && !strings.HasPrefix(c, "get mac-target ") {
			t.Fatalf("host transport issued a non-read call %q", c)
		}
	}

	// No-write effects: read-only tiers expose no stream, file tiers read
	// without modifying anything.
	for _, loc := range []ref.Locator{
		{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "kind-dev"},
		{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "team-ctx"},
		{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "novidesign"},
	} {
		p, err := scopedProvider(nil, loc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Open(context.Background(), loc); !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("%v: Open error=%v, want ErrUnsupported (no write-capable stream)", loc, err)
		}
	}
	wsLoc := ref.Locator{Scheme: "dir", Target: "src/a.txt", ScopeKind: "ws", ScopeValue: "issue-7"}
	wsP, err := scopedProvider(nil, wsLoc)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := wsP.Open(context.Background(), wsLoc)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := rc.Read(buf)
	rc.Close()
	if string(buf[:n]) != "in workspace\n" {
		t.Fatalf("ws read=%q, want workspace bytes", buf[:n])
	}
	hostLoc := ref.Locator{Scheme: "dir", Target: "src/a.txt", ScopeKind: "host", ScopeValue: "mac"}
	hostP, err := scopedProvider(nil, hostLoc)
	if err != nil {
		t.Fatal(err)
	}
	hrc, err := hostP.Open(context.Background(), hostLoc)
	if err != nil {
		t.Fatal(err)
	}
	hn, _ := hrc.Read(buf)
	hrc.Close()
	if string(buf[:hn]) != "hello" {
		t.Fatalf("host read=%q, want transport bytes", buf[:hn])
	}
	if raw, err := os.ReadFile(filepath.Join(wsSrc, "a.txt")); err != nil || string(raw) != "in workspace\n" {
		t.Fatalf("workspace file modified: %q err=%v", raw, err)
	}
}
