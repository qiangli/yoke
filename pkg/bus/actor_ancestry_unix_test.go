//go:build linux || darwin

package bus

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestExternalClaimSurvivesNestedShellWrappers(t *testing.T) {
	if os.Getenv("BASHY_ACTOR_NESTED_HELPER") == "1" {
		FleetNames = func() []string { return []string{"nested-agent"} }
		FleetResolveName = func(name string) string {
			if name == "nested-agent" {
				return name
			}
			return ""
		}
		DetectHarness = func() (string, bool) { return "codex", true }
		if got, err := ResolveAuthoredActor("nested-agent"); err != nil || got != "nested-agent" {
			t.Fatalf("nested-shell actor = %q, %v", got, err)
		}
		return
	}

	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := room.Join(room.Card{
		ID: "nested-agent", Nick: "nested-agent", Mode: "inbox", Tool: "codex",
		Binding: "codex:test", PID: os.Getpid(), OwnerPID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Two shells stand between this owner and the authored Bashy process. An
	// immediate-PPID comparison rejects it; the ancestry contract accepts it.
	script := `sh -c 'exec "$BASHY_ACTOR_TEST_EXE" -test.run=TestExternalClaimSurvivesNestedShellWrappers'`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"BASHY_ACTOR_NESTED_HELPER=1",
		"BASHY_ACTOR_TEST_EXE="+exe,
		"BASHY_PRINCIPAL=",
		"BASHY_ROOM_DIR="+os.Getenv("BASHY_ROOM_DIR"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nested helper: %v\n%s", err, out)
	}
}

func TestProcessAncestryRejectsDeadAnchor(t *testing.T) {
	if processHasAncestor(2147483000) {
		t.Fatal("dead anchor appeared in process ancestry")
	}
	if !processHasAncestor(os.Getpid()) || !processHasAncestor(os.Getppid()) {
		t.Fatalf("self/parent ancestry missing: self=%d parent=%s", os.Getpid(), strconv.Itoa(os.Getppid()))
	}
	if strings.TrimSpace(HashSessionClaim("")) != "" {
		t.Fatal("empty session id must not produce a claim")
	}
}

// The harness is found by NAME among this process's ancestors: the parent's
// own executable name finds the parent, and a name nobody above carries finds
// nothing.
func TestAncestorNamedFindsTheNearestAncestorByExecutableName(t *testing.T) {
	parent := os.Getppid()
	name, ok := processName(parent)
	if !ok {
		t.Skip("cannot read the parent's process name on this host")
	}
	pid, found := ancestorNamed("no-such-harness", strings.ToUpper(name))
	if !found || pid != parent {
		t.Fatalf("ancestorNamed(%q) = %d, %v; want the parent %d", name, pid, found, parent)
	}
	if pid, found := ancestorNamed("no-such-harness"); found {
		t.Fatalf("found pid %d for a name no ancestor has", pid)
	}
}
