package feed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// withStubNodeLauncher fakes the external/node seams for one test: Ensure
// returns scriptPath, and the launcher records the full argv it was asked to
// run, then execs a fake "node" that runs its $1 (proving launch order:
// node <script> args...).
func withStubNodeLauncher(t *testing.T, scriptPath string, provisioned *bool, launched *[]string) {
	t.Helper()
	oldEnsure, oldCmd := ensureNpmTool, npmToolCommand
	t.Cleanup(func() { ensureNpmTool, npmToolCommand = oldEnsure, oldCmd })

	ensureNpmTool = func(ctx context.Context, tool binmgr.Tool, version string) (string, error) {
		if version == "" {
			t.Error("node version must be pinned, got empty")
		}
		if tool.Name != "openclaw" {
			t.Errorf("provisioned tool = %q, want openclaw", tool.Name)
		}
		*provisioned = true
		return scriptPath, nil
	}
	npmToolCommand = func(ctx context.Context, version, script string, args ...string) (*exec.Cmd, error) {
		*launched = append([]string{"node@" + version, script}, args...)
		c := exec.CommandContext(ctx, script, args...)
		return c, nil
	}
}

func TestOpenClawDeclRuntime(t *testing.T) {
	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	// The tarball bundles 3 of 65 production deps and Windows cannot exec
	// .mjs: the declaration must route through the provisioned Node launcher.
	if spec.Tool == nil || spec.Tool.Runtime != "node" {
		t.Fatalf("tool runtime must be node, got %+v", spec.Tool)
	}
	if strings.TrimSpace(spec.Tool.RuntimeVersion) == "" {
		t.Fatal("tool runtime_version must pin the Node.js")
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	bad := *spec
	tool := *spec.Tool
	tool.Runtime = "deno"
	bad.Tool = &tool
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown runtime must fail validation, not silently exec bare")
	}
}

func TestNodeRuntimeSendLaunchesNodeScript(t *testing.T) {
	dir := t.TempDir()
	// The "script" is a directly-executable shell stand-in for openclaw.mjs;
	// argv[0] is the resolved script path by the time the launcher runs.
	script := filepath.Join(dir, "openclaw.mjs")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"messageId\":\"via-node\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var provisioned bool
	var launched []string
	withStubNodeLauncher(t, script, &provisioned, &launched)

	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	adapter, err := NewDeclAdapter(spec)
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m1",
		ConversationID: "telegram:123", SenderID: "s1", Text: "hi",
		ReceivedAt: time.Now()}
	ids, err := adapter.Send(context.Background(), []Envelope{env})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) != 1 || ids[0] != "via-node" {
		t.Fatalf("ids %v", ids)
	}
	if !provisioned {
		t.Fatal("node-runtime send skipped provisioning — deps would be missing")
	}
	if len(launched) < 3 || launched[1] != script {
		t.Fatalf("launcher argv = %v, want [node@<ver> %s ...]", launched, script)
	}
	if !strings.Contains(strings.Join(launched, " "), "message send") {
		t.Fatalf("launcher argv = %v, missing CLI args", launched)
	}
	// ResolveBin takes the same provisioned path.
	if p, err := adapter.ResolveBin(context.Background()); err != nil || p != script {
		t.Fatalf("ResolveBin = %q, %v; want provisioned %q", p, err, script)
	}
}

func TestNodeRuntimeBypassedByTestDoubles(t *testing.T) {
	// Tool.Bin (and binResolver) doubles are directly executable: they must
	// not trigger provisioning — otherwise every existing fake-based test
	// would need the network.
	dir := t.TempDir()
	fakeBin := realCLIFake(t, dir)

	var provisioned bool
	var launched []string
	withStubNodeLauncher(t, filepath.Join(dir, "unused"), &provisioned, &launched)

	spec, err := ParseDeclFile("openclaw.yaml")
	if err != nil {
		t.Fatalf("parse openclaw.yaml: %v", err)
	}
	spec.Tool.Bin = fakeBin
	adapter, err := NewDeclAdapter(spec)
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{Version: ContractVersion, Channel: "openclaw", MessageID: "m1",
		ConversationID: "telegram:123", SenderID: "s1", Text: "hi",
		ReceivedAt: time.Now()}
	ids, err := adapter.Send(context.Background(), []Envelope{env})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) != 1 || ids[0] != "mid-for-123" {
		t.Fatalf("ids %v", ids)
	}
	if provisioned || len(launched) != 0 {
		t.Fatal("test double triggered provisioning — fakes must stay offline")
	}
}
