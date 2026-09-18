package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// seedStore writes a conversation store for an agent and returns its directory.
func seedStore(t *testing.T, a fleet.Agent, files map[string]string) string {
	t.Helper()
	dir := agentStoreDir(a)
	if dir == "" {
		t.Fatal("no store dir derived — is HOME isolated?")
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestCloneContextBranchesTheStoreAndThenDiverges is the literal meaning of
// "shares context up to the point of cloning": the clone gets everything the
// parent knows NOW, and after that the two are unrelated — which is what makes
// running them in parallel safe, where running one agent twice is not.
func TestCloneContextBranchesTheStoreAndThenDiverges(t *testing.T) {
	isolateHome(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	parent := fleet.Agent{Name: "elif", Tool: "ycode", Model: "glm-5.2"}
	clone := fleet.Agent{Name: "elif2", Tool: "ycode", Model: "glm-5.2"}
	srcDir := seedStore(t, parent, map[string]string{
		"sessions/a.json": `{"turn":"before the clone"}`,
		"nested/deep/b":   "kept",
	})

	note, err := CloneAgentContext(parent, clone)
	if err != nil {
		t.Fatalf("clone context: %v", err)
	}
	if !strings.Contains(note, "inherited") {
		t.Errorf("note = %q, want it to report the branch", note)
	}

	dstDir := agentStoreDir(clone)
	if dstDir == srcDir {
		t.Fatal("clone and parent resolved to the SAME store — the clone would not be a second agent at all")
	}
	for rel, want := range map[string]string{
		"sessions/a.json": `{"turn":"before the clone"}`,
		"nested/deep/b":   "kept",
	} {
		got, err := os.ReadFile(filepath.Join(dstDir, rel))
		if err != nil {
			t.Fatalf("clone is missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}

	// DIVERGENCE: a turn the parent takes after the clone must not reach it.
	if err := os.WriteFile(filepath.Join(srcDir, "sessions", "c.json"), []byte(`{"turn":"after"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "sessions", "c.json")); err == nil {
		t.Error("the clone saw a turn the parent took AFTER cloning — the stores are not independent")
	}
}

// TestCloneContextRefusesALiveParent — a store being written cannot be copied
// coherently, and a clone silently seeded from a half-written transcript is the
// confidently-wrong-answer failure this model exists to prevent.
func TestCloneContextRefusesALiveParent(t *testing.T) {
	isolateHome(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	parent := fleet.Agent{Name: "elif", Tool: "ycode", Model: "glm-5.2"}
	seedStore(t, parent, map[string]string{"sessions/a.json": "{}"})

	if err := room.Join(room.Card{
		ID: "elif", Nick: "elif", Binding: "ycode:glm-5.2", Tool: "ycode",
		Mode: "interactive", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := CloneAgentContext(parent, fleet.Agent{Name: "elif2", Tool: "ycode", Model: "glm-5.2"})
	if err == nil {
		t.Fatal("cloning a LIVE agent's context must be refused, not silently torn")
	}
	if !strings.Contains(err.Error(), "--fresh") {
		t.Errorf("refusal should name the way forward, got: %v", err)
	}
}

func TestCloneContextRefusesDottedParentUnderCanonicalAndLegacyClaimKeys(t *testing.T) {
	for _, tc := range []struct {
		name, cardID string
	}{
		{name: "canonical", cardID: room.AgentClaimID("elif.agent")},
		{name: "legacy raw", cardID: "elif.agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			parent := fleet.Agent{Name: "elif.agent", Tool: "ycode", Model: "glm-5.2"}
			seedStore(t, parent, map[string]string{"sessions/a.json": "{}"})
			if err := room.Join(room.Card{
				ID: tc.cardID, Nick: parent.Name, Binding: "ycode:glm-5.2", Tool: "ycode",
				Mode: "interactive", PID: os.Getpid(),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := CloneAgentContext(parent, fleet.Agent{Name: "elif2", Tool: "ycode", Model: "glm-5.2"}); err == nil || !strings.Contains(err.Error(), "--fresh") {
				t.Fatalf("live dotted parent clone error = %v", err)
			}
		})
	}
}

// TestCloneContextIsFreshForAToolWeDoNotHostAStoreFor — bashy relocates only
// ycode's store. Every other harness keeps its history in its own home under its
// own naming, and reporting a branch we did not perform would be a lie in the
// one place the operator is deciding whether to re-brief.
func TestCloneContextIsFreshForAForeignTool(t *testing.T) {
	isolateHome(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	note, err := CloneAgentContext(
		fleet.Agent{Name: "bruno", Tool: "codex", Model: "gpt-5.5"},
		fleet.Agent{Name: "bruno2", Tool: "codex", Model: "gpt-5.5"},
	)
	if err != nil {
		t.Fatalf("a foreign tool must not fail the clone, got: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want empty so the caller renders the fresh-context reason", note)
	}
}

// TestCloneContextRefusesToMergeIntoAnExistingStore — merging two conversation
// histories would give the clone a past that never happened.
func TestCloneContextRefusesToMergeIntoAnExistingStore(t *testing.T) {
	isolateHome(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	parent := fleet.Agent{Name: "elif", Tool: "ycode", Model: "glm-5.2"}
	clone := fleet.Agent{Name: "elif2", Tool: "ycode", Model: "glm-5.2"}
	seedStore(t, parent, map[string]string{"sessions/a.json": "{}"})
	seedStore(t, clone, map[string]string{"sessions/its-own.json": "{}"})

	if _, err := CloneAgentContext(parent, clone); err == nil {
		t.Fatal("cloning onto an agent that already has a store must be refused")
	}
}
