package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wrapper's preservation commits (`weave(auto)`, `wip(weave #N)`) are made
// by the conductor process, so `git commit` inherited the CONDUCTOR's
// environment: BASHY_AGENT empty (-> "found identity unknown") or the sprint
// manager's (-> a mismatch against the story's assignee). Either way the
// commit-msg hook recorded a bypass and the sprint's manage score auto-failed,
// although the worker had committed its own story moments earlier. The
// wrapper commit preserves the WORKER's tree in the worker's workspace, so it
// must carry the worker's identity — the same one weaveChildEnv exports — and
// never the launcher's.
func TestMaybeAutoCommitStampsWorkerIdentityNeverLauncher(t *testing.T) {
	ws := weaveTestRepo(t)
	seen := filepath.Join(t.TempDir(), "seen")
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nprintf '%s' \"${BASHY_AGENT-unset}\" > " + seen + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_AGENT", "s467-manager")

	mustWrite(t, filepath.Join(ws, "work.txt"), "worker edit\n")
	if committed, err := maybeAutoCommitAs(ws, "weave(auto): issue 1 — test", "worker-fixture-agent"); err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	if got, _ := os.ReadFile(seen); string(got) != "worker-fixture-agent" {
		t.Fatalf("hook saw BASHY_AGENT=%q, want the worker's identity", got)
	}

	// Without a known worker the launcher's identity still must not leak: an
	// unattributed commit is honest, a manager-attributed one is wrong.
	mustWrite(t, filepath.Join(ws, "more.txt"), "more\n")
	if committed, err := maybeAutoCommit(ws, "weave(auto): issue 1 — test"); err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	if got, _ := os.ReadFile(seen); string(got) != "unset" {
		t.Fatalf("hook saw BASHY_AGENT=%q, want the launcher's identity dropped", got)
	}
}

// Environment identity counts only for the pending commit; a historical
// audit (`sprint commit-msg --check-range`, the pre-push hook) reads the
// Agent trailer alone. So the wrapper's commit must PERSIST the worker's
// attribution in its trailers, or every later audit reports it unknown.
func TestAutoCommitProvenanceCarriesWorkerAgent(t *testing.T) {
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	body := "Delivery commit trailers:\nSprint: #338\nStory: #1255\nStory-ID: 0baa2eb78731\n"
	for name, it := range map[string]*weaveItem{
		"owner":  {ID: 5, Title: "t", Body: body, Owner: "worker-fixture-agent"},
		"launch": {ID: 5, Title: "t", Body: body, Owner: "seat-a", LaunchSpec: &weaveLaunchSpec{Agent: "worker-fixture-agent"}},
	} {
		for kind, msg := range map[string]string{
			"clean":  weaveAutoCommitMessageWithContext(it, weaveTerminalEvidence{}),
			"killed": weaveCrashedAutoCommitMessage(it, 0, "wall-clock cap"),
		} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				trace, err := parseCommitTrace(msg)
				if err != nil {
					t.Fatalf("%v\n%s", err, msg)
				}
				if !trace.AgentPresent || trace.Agent != "worker-fixture-agent" {
					t.Fatalf("Agent trailer = %q (present=%v), want the worker\n%s", trace.Agent, trace.AgentPresent, msg)
				}
				if trace.Sprint != 338 || len(trace.Stories) != 1 || trace.Stories[0].ID != "0baa2eb78731" {
					t.Fatalf("provenance = %+v", trace)
				}
			})
		}
	}
	// No actor, no trailer: an invented identity is worse than none.
	msg := weaveCrashedAutoCommitMessage(&weaveItem{ID: 5, Title: "t", Body: body}, 1, "")
	if trace, err := parseCommitTrace(msg); err != nil || trace.AgentPresent {
		t.Fatalf("err=%v trace=%+v\n%s", err, trace, msg)
	}
	// A brief without trailers gets none — the Agent trailer rides with the
	// Sprint/Story block, never alone (the hook needs the block to resolve it).
	if msg := weaveCrashedAutoCommitMessage(&weaveItem{ID: 5, Title: "t", Owner: "worker-fixture-agent"}, 1, ""); strings.Contains(msg, "Agent:") {
		t.Fatalf("Agent trailer without a provenance block:\n%s", msg)
	}
}

// End to end through the real commit-msg verb: the wrapper commit for a story
// assigned to the worker, validated under the CONDUCTOR's environment (the
// manager's BASHY_AGENT), must resolve to the assignee and leave no bypass on
// the sprint thread.
func TestAutoCommitMessagePassesAttributionUnderLauncherEnv(t *testing.T) {
	_, dir, _ := commitAttributionFixture(t, "agent-a", true)
	it := &weaveItem{ID: 9, Title: "t", Owner: "agent-a", Body: "brief\n\nSprint: #87\nStory: #110\nStory-ID: d1e86f29d7a7\n"}
	t.Setenv("BASHY_AGENT", "s467-manager")
	for name, msg := range map[string]string{
		"clean":  weaveAutoCommitMessageWithContext(it, weaveTerminalEvidence{}),
		"killed": weaveCrashedAutoCommitMessage(it, 0, "signal terminated"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "message")
			if err := os.WriteFile(path, []byte(msg), 0o600); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := commitAttributionRun(path)
			if err != nil || stderr != "" {
				t.Fatalf("err=%v stderr=%s", err, stderr)
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			if events := findWeaveStory(q, 87).Thread; len(events) != 0 {
				t.Fatalf("bypass recorded for the worker's own preserved work: %+v", events)
			}
		})
	}
}
