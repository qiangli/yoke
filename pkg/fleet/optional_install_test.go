package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// Sprint 433 / story b79afba790fe: `service add --optional NAME` imports a
// shipped optional unit through the -f path, and `tool install` fetches a
// hidden kind:system recipe through binmgr. No network.

func TestServiceAddOptionalImportsMatrixChat(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	out := mustSvc(t, opts, "", "add", "--optional", "matrix-chat")
	if !strings.Contains(out, "matrix-chat") || !strings.Contains(out, "disabled") {
		t.Fatalf("add output = %q", out)
	}
	s, ok := New(opts...).Service("matrix-chat")
	if !ok {
		t.Fatal("matrix-chat not in the store after add --optional")
	}
	if s.Ring != ringLocal() {
		t.Fatalf("ring = %v, want local", s.Ring)
	}
	if s.Enabled {
		t.Fatal("optional unit imported enabled")
	}
	if len(s.Ports) != 1 || s.Ports[0].Bind != "127.0.0.1" {
		t.Fatalf("ports = %+v, want one loopback listener", s.Ports)
	}
	if s.Env["TUWUNEL_ALLOW_REGISTRATION"] != "false" || s.Env["TUWUNEL_ALLOW_FEDERATION"] != "false" {
		t.Fatalf("registration/federation not off: %v", s.Env)
	}

	// Identical re-add is a no-op, not a refusal.
	if out := mustSvc(t, opts, "", "add", "--optional", "matrix-chat"); !strings.Contains(out, "unchanged") {
		t.Fatalf("re-add output = %q, want unchanged", out)
	}
}

func TestServiceAddOptionalSetNameAndForce(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	bin := filepath.Join(t.TempDir(), "tuwunel")
	mustSvc(t, opts, "", "add", "--optional", "matrix-chat", "--set", "settings.binary="+bin)
	s, _ := New(opts...).Service("matrix-chat")
	if s.Settings["binary"] != bin || s.Settings["server_name"] != "localhost" {
		t.Fatalf("settings = %v", s.Settings)
	}
	// Same --set again: identical, a no-op.
	if out := mustSvc(t, opts, "", "add", "--optional", "matrix-chat", "--set", "settings.binary="+bin); !strings.Contains(out, "unchanged") {
		t.Fatalf("re-add output = %q", out)
	}
	// Different from the stored unit: refused without --force.
	_, err := svcRun(t, opts, "", "add", "--optional", "matrix-chat")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("differing re-add err = %v, want a --force refusal", err)
	}
	mustSvc(t, opts, "", "add", "--optional", "matrix-chat", "--force")
	if s, _ := New(opts...).Service("matrix-chat"); s.Settings["binary"] != "" {
		t.Fatalf("--force did not replace: settings = %v", s.Settings)
	}
	// --name stores the import under another name.
	mustSvc(t, opts, "", "add", "--optional", "matrix-chat", "--name", "chat2")
	if _, ok := New(opts...).Service("chat2"); !ok {
		t.Fatal("--name was not applied")
	}
}

func TestServiceAddOptionalRefusals(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	_, err := svcRun(t, opts, "", "add", "--optional", "nope")
	if err == nil || !strings.Contains(err.Error(), "matrix-chat") {
		t.Fatalf("unknown optional err = %v, want the available names", err)
	}
	f := filepath.Join(t.TempDir(), "u.yaml")
	body, _ := OptionalService("matrix-chat")
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svcRun(t, opts, "", "add", "-f", f, "--optional", "matrix-chat")
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("-f with --optional err = %v, want mutual exclusion", err)
	}
	if _, ok := New(opts...).Service("matrix-chat"); ok {
		t.Fatal("a refused add wrote the unit")
	}
	if got := OptionalServiceNames(); len(got) == 0 || got[0] != "matrix-chat" {
		t.Fatalf("OptionalServiceNames = %v", got)
	}
}

// The baseline tuwunel recipe is hidden, yet `tool install` resolves it.
func TestToolInstallResolvesHiddenSystemRecipe(t *testing.T) {
	tl, ok := baseline(t).Tool("tuwunel")
	if !ok || !tl.Hidden || tl.Kind != ToolKindSystem || !tl.IsManaged() {
		t.Fatalf("tuwunel = %+v, want a hidden managed system tool", tl)
	}
}

func TestToolInstallManagedSystemToolViaBinmgr(t *testing.T) {
	exe := []byte("#!/bin/sh\necho homeserver\n" + strings.Repeat("x", 2048))
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	asset := enc.EncodeAll(exe, nil)
	enc.Close()
	sum := sha256.Sum256(asset)

	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	oldTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay()}
	cat := New(opts...)
	if err := cat.SaveTool(Tool{
		Name: "homeserver", Kind: ToolKindSystem, Hidden: true,
		CLI: ToolCLI{Binary: "homeserver", Managed: &ToolManaged{
			Source: "github-release", Version: "v1.0.0", License: "Apache-2.0",
			Assets: map[string]ToolManagedAsset{binmgr.Platform(): {
				URL:    srv.URL + "/{version}-homeserver.zst",
				SHA256: hex.EncodeToString(sum[:]),
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	toolsDir := t.TempDir()
	out, err := runCmd(t, NewToolsCmd(opts...), "install", "homeserver", "--tools-dir", toolsDir)
	if err != nil {
		t.Fatalf("tool install: %v\n%s", err, out)
	}
	want := filepath.Join(toolsDir, "homeserver", "v1.0.0", binmgr.BinaryName("homeserver"))
	if out != "installed homeserver -> "+want+"\n" {
		t.Fatalf("output = %q, want the installed path line", out)
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, exe) {
		t.Fatalf("pinned copy is not the decompressed asset (err %v)", err)
	}
	if info, _ := os.Stat(want); info == nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatal("pinned copy is not executable")
	}
	tl, _ := New(opts...).Tool("homeserver")
	if tl.CLI.Binary != want {
		t.Fatalf("cli.binary = %q, want %q", tl.CLI.Binary, want)
	}
	if hits != 1 {
		t.Fatalf("downloads = %d, want 1", hits)
	}
}
