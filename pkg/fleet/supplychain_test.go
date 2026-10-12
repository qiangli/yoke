package fleet

import (
	"errors"
	"os"
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

func TestTuwunelIsAnUnverifiedServicePin(t *testing.T) {
	tl, ok := baseline(t).Tool("tuwunel")
	if !ok {
		t.Fatal("no tuwunel")
	}
	if m := tl.CLI.Managed; m == nil || !m.Service || m.Sigstore != nil || !m.Unverified() {
		t.Fatalf("tuwunel managed = %+v, want a service pin with sigstore pending", tl.CLI.Managed)
	}
	if err := tl.ServiceBuildCheck(); !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("ServiceBuildCheck = %v", err)
	}
}

func TestToolInstallRefusesUnverifiedServicePin(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	opts := serviceOpts(t.TempDir())
	for _, args := range [][]string{
		{"install", "tuwunel", "--tools-dir", t.TempDir()},
		{"install", "tuwunel", "--tools-dir", t.TempDir(), "--source", os.Args[0]},
	} {
		_, err := runCmd(t, NewToolsCmd(opts...), args...)
		if !errors.Is(err, ErrUnsignedServiceBuild) || !strings.Contains(err.Error(), "bashy only runs bashy-signed server builds") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
	if tl, _ := New(opts...).Tool("tuwunel"); tl.CLI.Binary != "tuwunel" {
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
	mustSvc(t, opts, "", "add", "--optional", "matrix-chat")
	s, _ := New(opts...).Service("matrix-chat")
	if s.Tool != "tuwunel" {
		t.Fatalf("matrix-chat tool = %q, want tuwunel", s.Tool)
	}
	_, err := svcRun(t, opts, "", "enable", "matrix-chat")
	if !errors.Is(err, ErrUnsignedServiceBuild) {
		t.Fatalf("enable err = %v", err)
	}
	if s, _ := New(opts...).Service("matrix-chat"); s.Enabled {
		t.Fatal("refused enable persisted enabled")
	}
	// The other write paths answer to the same gate.
	if _, err := svcRun(t, opts, "", "set", "matrix-chat", "--set", "enabled=true"); !errors.Is(err, ErrUnsignedServiceBuild) {
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
