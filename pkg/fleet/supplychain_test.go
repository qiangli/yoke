package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// Service supply-chain policy (Sprint 433): bashy starts only bashy-CI builds
// signed keylessly with Sigstore. A service pin without sigstore is
// unverified and refused at `tool install` and at service enable/start. No
// network: every refusal happens before binmgr is reached.

const (
	signedIdentity = "https://github.com/bashy/builds/.github/workflows/release.yml@refs/tags/v1.0.0"
	shaA           = "78628e6d13deda9e4832aeb93f2d10332e3c4e70a8815b45e6ef773404edd5a3"
)

func servicePin(sig *ToolSigstore) *ToolManaged {
	return &ToolManaged{
		Source: "github-release", Version: "v1.0.0", License: "Apache-2.0", Service: true, Sigstore: sig,
		Assets: map[string]ToolManagedAsset{
			"linux/amd64": {URL: "https://github.com/bashy/builds/releases/download/{version}/svc-linux-amd64", SHA256: shaA},
			"linux/arm64": {URL: "https://github.com/bashy/builds/releases/download/{version}/svc-linux-arm64", SHA256: shaA},
		},
	}
}

const (
	tuwunelIdentity = "https://github.com/qiangli/tuwunel/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.9.4-2"
	tuwunelRelease  = "https://github.com/qiangli/tuwunel/releases/download/bashy-v1.9.4-2/"
)

// The first bashy-signed tuwunel build: a VERIFIED service pin — the exact
// workflow identity and issuer, and every platform asset carries a sha256 and
// a Sigstore bundle beside it.
func TestTuwunelIsAVerifiedServicePin(t *testing.T) {
	tl, ok := baseline(t).Tool("tuwunel")
	if !ok {
		t.Fatal("no tuwunel")
	}
	m := tl.CLI.Managed
	if m == nil || !m.Service || m.Sigstore == nil || m.Unverified() {
		t.Fatalf("tuwunel managed = %+v, want a signed service pin", m)
	}
	if m.Sigstore.Identity != tuwunelIdentity || m.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("sigstore = %+v", m.Sigstore)
	}
	if err := tl.ServiceBuildCheck(); err != nil {
		t.Fatalf("ServiceBuildCheck = %v", err)
	}
	bt, err := tl.BinmgrTool()
	if err != nil {
		t.Fatal(err)
	}
	if bt.Sigstore == nil || bt.Sigstore.Identity != tuwunelIdentity || bt.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("binmgr sigstore = %+v", bt.Sigstore)
	}
	want := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}
	if len(bt.Assets) != len(want) {
		t.Fatalf("assets = %d, want %v", len(bt.Assets), want)
	}
	for _, k := range want {
		a, ok := bt.Assets[k]
		if !ok {
			t.Fatalf("no %s asset", k)
		}
		if wantURL := tuwunelRelease + "tuwunel-" + strings.ReplaceAll(k, "/", "-"); a.URL != wantURL {
			t.Errorf("%s: url %q, want %q", k, a.URL, wantURL)
		}
		if len(a.SHA256) != 64 || a.SHA512 != "" {
			t.Errorf("%s: sha256 %q sha512 %q", k, a.SHA256, a.SHA512)
		}
		if a.SigstoreBundle != a.URL+binmgr.DefaultBundleSuffix {
			t.Errorf("%s: bundle %q", k, a.SigstoreBundle)
		}
		if a.Binary != "" || a.Tree {
			t.Errorf("%s: want a raw executable, got member %q tree %v", k, a.Binary, a.Tree)
		}
	}
}

// saveUnsignedSvc stores a synthetic service pin WITHOUT sigstore — the shape
// the supply-chain gate must keep refusing now that tuwunel is signed.
func saveUnsignedSvc(t *testing.T, opts []Option) {
	t.Helper()
	if err := New(opts...).SaveTool(Tool{Name: "unsignedsvc", Kind: ToolKindSystem, Hidden: true,
		CLI: ToolCLI{Binary: "unsignedsvc", Managed: servicePin(nil)}}); err != nil {
		t.Fatal(err)
	}
	tl, _ := New(opts...).Tool("unsignedsvc")
	if !tl.CLI.Managed.Unverified() {
		t.Fatal("synthetic pin is not unverified")
	}
}

func TestToolInstallRefusesUnverifiedServicePin(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	opts := serviceOpts(t.TempDir())
	saveUnsignedSvc(t, opts)
	for _, args := range [][]string{
		{"install", "unsignedsvc", "--tools-dir", t.TempDir()},
		{"install", "unsignedsvc", "--tools-dir", t.TempDir(), "--source", os.Args[0]},
	} {
		_, err := runCmd(t, NewToolsCmd(opts...), args...)
		if !errors.Is(err, ErrUnsignedServiceBuild) || !strings.Contains(err.Error(), "bashy only runs bashy-signed server builds") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
	if tl, _ := New(opts...).Tool("unsignedsvc"); tl.CLI.Binary != "unsignedsvc" {
		t.Fatalf("a refused install rewrote cli.binary to %q", tl.CLI.Binary)
	}
}

func TestToolInstallRefusesSourceForSignedServicePin(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	cat := New(opts...)
	if err := cat.SaveTool(Tool{Name: "signedsvc", Kind: ToolKindSystem, Hidden: true,
		CLI: ToolCLI{Binary: "signedsvc", Managed: servicePin(&ToolSigstore{Identity: signedIdentity, Issuer: binmgr.GitHubActionsIssuer})}}); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, NewToolsCmd(opts...), "install", "signedsvc", "--source", os.Args[0], "--tools-dir", t.TempDir())
	if !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceEnableRefusesUnverifiedBuild(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	saveUnsignedSvc(t, opts)
	body, err := OptionalService("matrix-chat")
	if err != nil {
		t.Fatal(err)
	}
	unit := strings.Replace(strings.Replace(string(body), "\ntool: tuwunel\n", "\ntool: unsignedsvc\n", 1), "\nname: matrix-chat\n", "\nname: unsigned-chat\n", 1)
	f := filepath.Join(t.TempDir(), "u.yaml")
	if err := os.WriteFile(f, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSvc(t, opts, "", "add", "-f", f)
	s, _ := New(opts...).Service("unsigned-chat")
	if s.Tool != "unsignedsvc" {
		t.Fatalf("unit tool = %q, want unsignedsvc", s.Tool)
	}
	_, err = svcRun(t, opts, "", "enable", "unsigned-chat")
	if !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("enable err = %v", err)
	}
	if s, _ := New(opts...).Service("unsigned-chat"); s.Enabled {
		t.Fatal("refused enable persisted enabled")
	}
	// The other write paths answer to the same gate.
	if _, err := svcRun(t, opts, "", "set", "unsigned-chat", "--set", "enabled=true"); !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("set enabled err = %v", err)
	}
	if err := New(opts...).CheckServiceBuild(s); !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("CheckServiceBuild (the start gate) = %v", err)
	}
	// A unit naming no tool is not affected.
	if err := New(opts...).CheckServiceBuild(Service{Name: "web"}); err != nil {
		t.Fatal(err)
	}
}

// The shipped matrix-chat unit now passes the start gate: its tuwunel pin is
// the bashy-signed build.
func TestMatrixChatPassesServiceBuildGate(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	mustSvc(t, opts, "", "add", "--optional", "matrix-chat")
	s, _ := New(opts...).Service("matrix-chat")
	if s.Tool != "tuwunel" {
		t.Fatalf("matrix-chat tool = %q, want tuwunel", s.Tool)
	}
	if err := New(opts...).CheckServiceBuild(s); err != nil {
		t.Fatalf("CheckServiceBuild = %v", err)
	}
}

func TestSignedServicePinPassesGateAndCarriesSigstore(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	cat := New(opts...)
	if err := cat.SaveTool(Tool{Name: "signedsvc", Kind: ToolKindSystem, Hidden: true,
		CLI: ToolCLI{Binary: "signedsvc", Managed: servicePin(&ToolSigstore{Identity: signedIdentity, Issuer: binmgr.GitHubActionsIssuer})}}); err != nil {
		t.Fatal(err)
	}
	tl, _ := New(opts...).Tool("signedsvc")
	if err := tl.ServiceBuildCheck(); err != nil {
		t.Fatal(err)
	}
	if err := New(opts...).CheckServiceBuild(Service{Name: "svc", Tool: "signedsvc"}); err != nil {
		t.Fatal(err)
	}
	bt, err := tl.BinmgrTool()
	if err != nil {
		t.Fatal(err)
	}
	if bt.Sigstore == nil || bt.Sigstore.Identity != signedIdentity || bt.Sigstore.Issuer != binmgr.GitHubActionsIssuer {
		t.Fatalf("binmgr sigstore = %+v", bt.Sigstore)
	}
	a := bt.Assets["linux/amd64"]
	if a.SigstoreBundle != "https://github.com/bashy/builds/releases/download/v1.0.0/svc-linux-amd64.sigstore.json" {
		t.Fatalf("bundle = %q", a.SigstoreBundle)
	}

	// Bundle forms: suffix, and an https URL with {asset}/{version}.
	for bundle, want := range map[string]string{
		".bundle": "https://github.com/bashy/builds/releases/download/v1.0.0/svc-linux-amd64.bundle",
		"https://sig.example/{version}/{asset}.sigstore.json": "https://sig.example/v1.0.0/svc-linux-amd64.sigstore.json",
	} {
		tl := Tool{Name: "s", CLI: ToolCLI{Managed: servicePin(&ToolSigstore{Identity: signedIdentity, Issuer: binmgr.GitHubActionsIssuer, Bundle: bundle})}}
		bt, err := tl.BinmgrTool()
		if err != nil {
			t.Fatal(err)
		}
		if got := bt.Assets["linux/amd64"].SigstoreBundle; got != want {
			t.Errorf("bundle %q -> %q, want %q", bundle, got, want)
		}
	}
}

func TestSigstorePinValidation(t *testing.T) {
	ok := ToolSigstore{Identity: signedIdentity, Issuer: binmgr.GitHubActionsIssuer}
	cases := map[string]func(*ToolManaged){
		"wrong issuer":       func(m *ToolManaged) { m.Sigstore.Issuer = "https://accounts.google.com" },
		"identity not exact": func(m *ToolManaged) { m.Sigstore.Identity = "bashy/builds" },
		"identity no ref": func(m *ToolManaged) {
			m.Sigstore.Identity = "https://github.com/bashy/builds/.github/workflows/release.yml"
		},
		"one bundle many":  func(m *ToolManaged) { m.Sigstore.Bundle = "https://sig.example/one.json" },
		"bundle not https": func(m *ToolManaged) { m.Sigstore.Bundle = "http://sig.example/{asset}" },
		"signed without sha256": func(m *ToolManaged) {
			a := m.Assets["linux/amd64"]
			a.SHA256, a.SHA512 = "", strings.Repeat("a", 128)
			m.Assets["linux/amd64"] = a
		},
	}
	for name, mutate := range cases {
		s := ok
		m := servicePin(&s)
		mutate(m)
		if err := m.Validate("x"); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	s := ok
	if err := servicePin(&s).Validate("x"); err != nil {
		t.Fatal(err)
	}
}

// A non-service pin without sigstore keeps the sha256-only path untouched.
func TestNonServicePinUnchanged(t *testing.T) {
	m := servicePin(nil)
	m.Service = false
	tl := Tool{Name: "plain", Kind: ToolKindSystem, CLI: ToolCLI{Managed: m}}
	if m.Unverified() {
		t.Fatal("a non-service pin is not 'unverified'")
	}
	bt, err := tl.BinmgrTool()
	if err != nil {
		t.Fatal(err)
	}
	if bt.Sigstore != nil || bt.Assets["linux/amd64"].SigstoreBundle != "" {
		t.Fatalf("non-service pin grew sigstore: %+v", bt)
	}
}
