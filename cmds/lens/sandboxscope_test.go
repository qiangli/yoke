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

// sandboxPeers mirrors `outpost peers status --json`: Dragon is the local
// dev-loop host (also reachable by alias), novidesign a remote
// Podman-backed peer with one saved SSH target, lern another remote peer
// with no saved target.
const sandboxPeers = `[
 {"host":"dragon","alias":"dbox","online":true},
 {"host":"novidesign","online":true},
 {"host":"lern","online":true}]`

const sandboxTargets = `[{"name":"dogtest","host":"novidesign"},{"name":"dragonsftp","host":"dragon"}]`

const sandboxPodman = `[{"Id":"c1","Names":["web"],"Image":"img:web","State":"running"}]`

// sandboxFixture injects canned outpost peers, SSH targets and podman
// output; no live peer is contacted. It records every runner call so tests
// can prove refused locators never execute.
func sandboxFixture(t *testing.T) (peers, targets *int, argv *[]string) {
	t.Helper()
	var peerCalls, targetCalls int
	var got []string
	oldP, oldT, oldR := peerRunner, hostTargetsRunner, sandboxRunner
	peerRunner = func(context.Context) ([]byte, error) {
		peerCalls++
		return []byte(sandboxPeers), nil
	}
	hostTargetsRunner = func(context.Context) ([]byte, error) {
		targetCalls++
		return []byte(sandboxTargets), nil
	}
	sandboxRunner = func(_ context.Context, c string, a ...string) ([]byte, error) {
		got = append([]string{c}, a...)
		return []byte(sandboxPodman), nil
	}
	t.Cleanup(func() { peerRunner, hostTargetsRunner, sandboxRunner = oldP, oldT, oldR })
	return &peerCalls, &targetCalls, &got
}

// legacySandboxCwd lays out the retired local weave sandboxes directory
// under a temp HOME and returns an unrelated cwd. Nothing may read the
// legacy dir: the sandbox tier is remote Podman only.
func legacySandboxCwd(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	box := filepath.Join(home, ".bashy", "weave", "repo-0000abcd", "sandboxes", "box-1", "src")
	if err := os.MkdirAll(box, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(box, "legacy.txt"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return t.TempDir()
}

// TestTierSandboxRefusesDragonLocal pins the never-Dragon rule: the literal
// Dragon id, its case variant, a Dragon alias and the retired local
// directory ids all fail closed before any podman command runs.
func TestTierSandboxRefusesDragonLocal(t *testing.T) {
	peers, _, argv := sandboxFixture(t)
	cwd := legacySandboxCwd(t)
	for _, op := range []string{
		"oci:containers@sandbox:dragon",
		"oci:containers@sandbox:DRAGON",
		"oci:containers@sandbox:dbox",
	} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", op)
		if code == 0 || out != "" || !strings.Contains(errOut, "never Dragon") {
			t.Fatalf("%s: code %d out %q stderr %q, want never-Dragon refusal", op, code, out, errOut)
		}
	}
	// The retired local directory ids are refused too: no local weave
	// sandboxes directory is consulted anymore.
	for _, op := range []string{
		"dir:src@sandbox:box-1",
		"dir:src@sandbox:repo-0000abcd%2Fbox-1",
	} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", op)
		if code == 0 || out != "" {
			t.Fatalf("%s: code %d out %q stderr %q, want refusal", op, code, out, errOut)
		}
		if strings.Contains(out, "legacy.txt") || strings.Contains(errOut, "legacy") {
			t.Fatalf("%s: touched the retired local sandbox dir", op)
		}
	}
	if *peers == 0 {
		t.Fatal("expected the alias case to reach peer resolution")
	}
	if len(*argv) != 0 {
		t.Fatalf("refused locators executed podman: %v", *argv)
	}
}

// TestTierSandboxPodmanReadOnly pins the novidesign contract: the explicit
// peer resolves to exactly one saved SSH target, the podman listing travels
// as literal outpost argv, emitted refs keep the @sandbox:novidesign scope,
// and no write-capable stream exists.
func TestTierSandboxPodmanReadOnly(t *testing.T) {
	_, _, argv := sandboxFixture(t)
	cwd := t.TempDir()
	out, errOut, code := runTool(t, cwd, "", "-t", "graph", "oci:containers@sandbox:novidesign")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "web") || !strings.Contains(out, `"oci:containers/c1@sandbox:novidesign"`) {
		t.Fatalf("rows lost the sandbox scope:\n%s", out)
	}
	if want := []string{"outpost", "ssh", "exec", "dogtest", "--", "podman", "ps", "--all", "--format", "json"}; !reflect.DeepEqual(*argv, want) {
		t.Fatalf("argv=%v want %v", *argv, want)
	}
	// Only the transport prefix may use exec: past the "--" separator the
	// podman verb must be the read-only ps, with no mutating verb present.
	sep := -1
	for i, tok := range *argv {
		if tok == "--" {
			sep = i
		}
	}
	tail := (*argv)[sep+1:]
	if len(tail) < 2 || tail[0] != "podman" || tail[1] != "ps" {
		t.Fatalf("podman argv tail=%v, want podman ps ...", tail)
	}
	for _, tok := range tail {
		for _, w := range []string{"create", "run", "start", "stop", "rm", "rmi", "pull", "push", "build", "kill", "restart", "pause", "unpause", "commit", "rename", "put"} {
			if tok == w {
				t.Fatalf("argv %v contains write verb %q", *argv, w)
			}
		}
	}

	loc := ref.Locator{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "novidesign"}
	p, err := scopedProvider(nil, loc)
	if err != nil {
		t.Fatal(err)
	}
	// The scoped router and the provider must agree: no duplicate registry.
	if _, err := p.List(context.Background(), loc); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(context.Background(), loc); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Open error=%v, want ErrUnsupported (no write-capable stream)", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "containers"}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("unscoped list error=%v, want ErrUnsupported", err)
	}
	if _, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "lern"}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("other-peer list error=%v, want ErrUnsupported", err)
	}
}

// TestTierSandboxExplicitScope pins no-ambient-fallback at the CLI layer:
// only oci serves @sandbox, unknown/offline/untargeted peers fail before
// any podman command runs, and the unscoped oci recipe never touches the
// sandbox transport.
func TestTierSandboxExplicitScope(t *testing.T) {
	_, _, argv := sandboxFixture(t)
	for _, loc := range []ref.Locator{
		{Scheme: "dir", Target: "src", ScopeKind: "sandbox", ScopeValue: "novidesign"},
		{Scheme: "k8s", Target: "pods", ScopeKind: "sandbox", ScopeValue: "novidesign"},
		{Scheme: "cloud", Target: "droplets", ScopeKind: "sandbox", ScopeValue: "novidesign"},
		{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "nobody"},
		{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "lern"},
		{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "-bad"},
		{Scheme: "oci", Target: "containers", ScopeKind: "sandbox", ScopeValue: "a b"},
	} {
		if _, err := scopedProvider(nil, loc); err == nil {
			t.Errorf("%v: scoped pull accepted", loc)
		}
	}
	if len(*argv) != 0 {
		t.Fatalf("refused locators executed podman: %v", *argv)
	}
	// Unscoped oci stays the local recipe and never reaches the sandbox runner.
	if _, err := providers["oci"](nil, ref.Locator{Scheme: "oci", Target: "containers"}); err != nil {
		t.Fatal(err)
	}
	if len(*argv) != 0 {
		t.Fatalf("unscoped oci reached the sandbox transport: %v", *argv)
	}
}
