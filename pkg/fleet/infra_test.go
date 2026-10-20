package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/provider"
)

// First red test (S472.12): a fixture provider manifest in a temp local ring
// overrides the embedded one of the same name, and the resolved entry's
// origin must equal local.
func TestProviderKindRingPrecedence(t *testing.T) {
	isolatedFleetRoot(t)

	base, ok := New(WithoutCloudOverlay()).InfraProvider("digitalocean")
	if !ok {
		t.Fatal("embedded digitalocean provider missing")
	}
	if base.Ring != assetring.RingEmbedded {
		t.Fatalf("embedded origin = %v, want embedded", base.Ring)
	}
	if !base.Builtin {
		t.Fatal("embedded digitalocean entry must carry the loader-level builtin marker")
	}

	local := []byte("kind: infra\nname: digitalocean\nform: declarative\nprotocol_version: provider/v1\nversion: local-override\ncapabilities: [create]\ndeclarative:\n  lifecycle:\n    create: {endpoint: /v2/droplets}\n")
	cat := New(WithoutCloudOverlay())
	dir := cat.nounDir(dirInfra)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "digitalocean.yaml"), local, 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := New(WithoutCloudOverlay()).InfraProvider("digitalocean")
	if !ok {
		t.Fatal("digitalocean provider not found")
	}
	if got.Ring != assetring.RingLocal {
		t.Fatalf("origin = %v, want local", got.Ring)
	}
	if got.Version != "local-override" {
		t.Fatalf("version = %q, want the local override", got.Version)
	}
}

// A well-formed manifest with the wrong protocol version fails with a
// distinct error kind (and exit code) from an invalid manifest.
func TestProviderManifestProtocolMismatch(t *testing.T) {
	isolatedFleetRoot(t)
	body := []byte("kind: infra\nname: badproto\nform: declarative\nprotocol_version: provider/v9\nversion: \"1\"\ncapabilities: [create]\ndeclarative:\n  lifecycle:\n    create: {endpoint: /x}\n")
	_, err := ParseInfraProvider("badproto", body, nil)
	var ie *InfraError
	if !errors.As(err, &ie) || ie.Kind != InfraErrProtocolMismatch {
		t.Fatalf("err = %v, want protocol_mismatch", err)
	}
	if got := InfraExitCode(err); got != ExitInfraProtocolMismatch {
		t.Fatalf("exit = %d, want %d", got, ExitInfraProtocolMismatch)
	}

	invalid := []byte("kind: infra\nname: badform\nform: telepathy\nprotocol_version: provider/v9\ncapabilities: [create]\n")
	_, err = ParseInfraProvider("badform", invalid, nil)
	if !errors.As(err, &ie) || ie.Kind != InfraErrInvalid {
		t.Fatalf("err = %v, want invalid_manifest", err)
	}
	if got := InfraExitCode(err); got != ExitInfraInvalid {
		t.Fatalf("exit = %d, want %d", got, ExitInfraInvalid)
	}
	if ExitInfraInvalid == ExitInfraProtocolMismatch {
		t.Fatal("exit codes must be distinct")
	}

	for _, carrying := range []string{
		"kind: infra\nname: creds\nform: declarative\nprotocol_version: provider/v1\ncapabilities: [create]\ntoken: hunter2\ndeclarative:\n  lifecycle:\n    create: {endpoint: /x}\n",
		"kind: infra\nname: creds\nform: declarative\nprotocol_version: provider/v1\ncapabilities: [create]\napi_key: hunter2\ndeclarative:\n  lifecycle:\n    create: {endpoint: /x}\n",
	} {
		if _, err := ParseInfraProvider("creds", []byte(carrying), nil); err == nil {
			t.Fatal("manifest carrying credential values must be rejected")
		} else if !errors.As(err, &ie) || ie.Kind != InfraErrInvalid {
			t.Fatalf("credential err = %v, want invalid_manifest", err)
		}
	}
}

// Argv with shell metacharacters is passed literally: resolution never joins,
// interpolates, or evaluates through a shell.
func TestProviderArgvNoShell(t *testing.T) {
	isolatedFleetRoot(t)
	want := []string{"/usr/local/bin/do-plugin", "--name", "x; rm -rf /", "$(evil)", "`back`", "a|b", "semi;colon"}
	body := "kind: infra\nname: shellish\nform: executable\nprotocol_version: provider/v1\ncapabilities: [create]\nargv: [\"/usr/local/bin/do-plugin\", \"--name\", \"x; rm -rf /\", \"$(evil)\", \"`back`\", \"a|b\", \"semi;colon\"]\n"
	cat := New(WithoutCloudOverlay())
	if err := os.MkdirAll(cat.nounDir(dirInfra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cat.nounDir(dirInfra), "shellish.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	res, err := New(WithoutCloudOverlay()).ResolveInfra(context.Background(), "shellish",
		validCreateReq(),
		InfraResolveOptions{AllowExec: true, Exec: func(_ context.Context, argv []string, req provider.Request) (provider.Result, error) {
			got = append([]string(nil), argv...)
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q (literally)", got, want)
	}
	if res.Result == nil || res.Result.Op != provider.OpCreate {
		t.Fatalf("result = %+v, want the hook outcome", res.Result)
	}
}

// A declarative fixture whose templates contain shell metacharacters loads,
// validates, and resolves with zero process execs: the hook fails the test
// if it is ever called.
func TestDeclarativeManifestLoadsWithoutExec(t *testing.T) {
	isolatedFleetRoot(t)
	body := []byte("kind: infra\nname: decl\nform: declarative\nprotocol_version: provider/v1\nversion: \"2\"\ncapabilities: [create, list]\ndeclarative:\n  endpoints:\n    create: /v2/things?name=$(name)&q=a;b\n  templates:\n    body: '{\"x\": \"$(evil); drop\"}'\n  lifecycle:\n    create: {endpoint: /v2/things, template: body}\n    list: {endpoint: /v2/things}\n")
	p, err := ParseInfraProvider("decl", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Form != provider.FormDeclarative {
		t.Fatalf("form = %q", p.Manifest.Form)
	}
	cat := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	if err := os.MkdirAll(cat.nounDir(dirInfra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cat.nounDir(dirInfra), "decl.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	hook := func(context.Context, []string, provider.Request) (provider.Result, error) {
		t.Fatal("exec hook must never run for a declarative manifest")
		return provider.Result{}, nil
	}
	res, err := cat.ResolveInfra(context.Background(), "decl",
		validCreateReq(),
		InfraResolveOptions{Exec: hook})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider.Manifest.Name != "decl" || res.Result != nil || res.Argv != nil {
		t.Fatalf("resolution = %+v, want declarative entry with no argv and no result", res)
	}
}

// In a no-exec context an executable plugin is refused with the distinct
// provider.ErrExecDisabled error and is never started.
func TestNoExecContextRefusesExecutablePlugin(t *testing.T) {
	isolatedFleetRoot(t)
	body := []byte("kind: infra\nname: plug\nform: executable\nprotocol_version: provider/v1\ncapabilities: [create]\nargv: [\"/bin/plug\", \"create\"]\n")
	cat := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	if err := os.MkdirAll(cat.nounDir(dirInfra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cat.nounDir(dirInfra), "plug.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []InfraResolveOptions{
		{},
		{AllowExec: false, Exec: func(context.Context, []string, provider.Request) (provider.Result, error) {
			t.Fatal("exec hook must never run in a no-exec context")
			return provider.Result{}, nil
		}},
	} {
		_, err := cat.ResolveInfra(context.Background(), "plug",
			validCreateReq(), opts)
		var perr *provider.Error
		if !errors.As(err, &perr) || perr.Code != provider.ErrExecDisabled {
			t.Fatalf("err = %v, want exec_disabled", err)
		}
	}
}

// Offline load from the cached cloud ring: a fixture written into the cache
// directory resolves with origin cloud and no network involved.
func TestProviderCloudCacheOfflineLoad(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"BASHY_HOME", "BASHY_FLEET_DIR", "BASHY_INFRA_DIR", "BASHY_INFRA_PATH"} {
		t.Setenv(key, t.TempDir())
	}
	dir := filepath.Join(CloudCacheRoot(root), dirInfra)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("kind: infra\nname: cached\nform: declarative\nprotocol_version: provider/v1\nversion: cached-1\ncapabilities: [list]\ndeclarative:\n  lifecycle:\n    list: {endpoint: /v2/things}\n")
	if err := os.WriteFile(filepath.Join(dir, "cached.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := New(WithRoot(root), WithoutLocalStore()).InfraProvider("cached")
	if !ok {
		t.Fatal("cached cloud entry not found")
	}
	if got.Ring != assetring.RingCloud {
		t.Fatalf("origin = %v, want cloud", got.Ring)
	}
	if got.Version != "cached-1" {
		t.Fatalf("version = %q", got.Version)
	}
}

// The infra kind rides the kind-core write surface: add/import, show, list,
// and rm around a temp local ring.
func TestInfraKindCoreVerbs(t *testing.T) {
	isolatedFleetRoot(t)
	file := filepath.Join(t.TempDir(), "mine.yaml")
	body := "kind: infra\nname: mine\nform: executable\nprotocol_version: provider/v1\ncapabilities: [create, destroy]\nargv: [\"/bin/mine\"]\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithRoot(filepath.Join(t.TempDir())), WithoutCloudOverlay()}
	add := newAdd(KindInfra, opts)
	add.SetArgs([]string{file})
	add.SetOut(os.Stdout)
	if err := add.Execute(); err != nil {
		t.Fatalf("add: %v", err)
	}
	show := newShow(KindInfra, opts)
	show.SetArgs([]string{"mine"})
	var out strings.Builder
	show.SetOut(&out)
	if err := show.Execute(); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "mine") || !strings.Contains(out.String(), "executable") {
		t.Fatalf("show = %q", out.String())
	}
	rm := newRm(KindInfra, opts, (*Catalog).RemoveInfraProvider)
	rm.SetArgs([]string{"mine"})
	rm.SetOut(&out)
	if err := rm.Execute(); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, ok := New(opts...).InfraProvider("mine"); ok {
		t.Fatal("rm did not remove the local entry")
	}
}

func validCreateReq() provider.Request {
	return provider.Request{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate, IdempotencyKey: "k1", Spec: &provider.Spec{Name: "x"}}
}

func writeInfraFixture(t *testing.T, name, body string) *Catalog {
	t.Helper()
	isolatedFleetRoot(t)
	cat := New(WithoutCloudOverlay())
	if err := os.MkdirAll(cat.nounDir(dirInfra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cat.nounDir(dirInfra), name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(WithoutCloudOverlay())
}

func TestInfraUnsupportedOpIsExplicit(t *testing.T) {
	cat := writeInfraFixture(t, "nostop", "kind: infra\nname: nostop\nform: executable\nprotocol_version: provider/v1\ncapabilities: [create]\nargv: [\"/bin/plug\"]\n")
	calls := 0
	hook := func(context.Context, []string, provider.Request) (provider.Result, error) {
		calls++
		return provider.Result{}, nil
	}
	req := provider.Request{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpStop, IdempotencyKey: "k", ResourceID: "r1"}
	_, err := cat.ResolveInfra(context.Background(), "nostop", req, InfraResolveOptions{AllowExec: true, Exec: hook})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.ErrUnsupportedCapability {
		t.Fatalf("err = %v, want unsupported_capability", err)
	}
	if calls != 0 {
		t.Fatalf("exec hook called %d times, want 0", calls)
	}
}

func TestInfraProtocolMismatchRequestRefused(t *testing.T) {
	cat := writeInfraFixture(t, "plug", "kind: infra\nname: plug\nform: executable\nprotocol_version: provider/v1\ncapabilities: [create]\nargv: [\"/bin/plug\"]\n")
	calls := 0
	hook := func(context.Context, []string, provider.Request) (provider.Result, error) {
		calls++
		return provider.Result{}, nil
	}
	req := validCreateReq()
	req.ProtocolVersion = "provider/v0"
	_, err := cat.ResolveInfra(context.Background(), "plug", req, InfraResolveOptions{AllowExec: true, Exec: hook})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.ErrProtocolMismatch {
		t.Fatalf("err = %v, want protocol_mismatch", err)
	}
	if calls != 0 {
		t.Fatalf("exec hook called %d times, want 0", calls)
	}
}
