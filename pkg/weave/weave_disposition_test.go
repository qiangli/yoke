package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sprint 224 S2: one guarded teardown, an explicit disposition, nothing unique
// lost. The fixture plants every artifact kind a run owns so the test can
// assert each one is gone (or kept) by name.

func plantRunArtifacts(t *testing.T, dir string, id int64) map[string]string {
	t.Helper()
	it := &weaveItem{ID: id}
	paths := map[string]string{}
	for _, kind := range []string{"socket", "log", "cache", "agent-data"} {
		var p string
		switch kind {
		case "socket":
			p = weaveCtlSockPath(dir, id)
		case "log":
			p = filepath.Join(dir, "logs", "issue-1.log")
		default:
			p = weaveRunArtifactPath(dir, it, kind)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if kind == "cache" || kind == "agent-data" {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			p2 := filepath.Join(p, "blob")
			if err := os.WriteFile(p2, []byte("bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[kind] = p
	}
	return paths
}

func TestWeaveAbandonRejectedPreservesTipAndTearsDownEverything(t *testing.T) {
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	root, workspace, sha := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	planted := plantRunArtifacts(t, dir, 1)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
		LogPath: planted["log"], CtlSock: planted["socket"],
	}}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "abandon", "1", "--yes", "--json", "--disposition", "rejected", "--reason", "gate red twice")
	if code != 0 {
		t.Fatalf("abandon --disposition rejected: exit=%d %s", code, out)
	}
	var env struct {
		Data struct {
			Disposition  string `json:"disposition"`
			PreservedRef string `json:"preserved_ref"`
			Leftovers    []any  `json:"leftovers"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if env.Data.Disposition != "rejected" || env.Data.PreservedRef == "" || len(env.Data.Leftovers) != 0 {
		t.Fatalf("unexpected result: %+v\n%s", env.Data, out)
	}
	if got := gitT(t, root, "rev-parse", env.Data.PreservedRef); got != sha {
		t.Fatalf("salvage ref %s = %s, want %s", env.Data.PreservedRef, got, sha)
	}
	for kind, p := range map[string]string{"workspace": workspace, "socket": planted["socket"], "log": planted["log"], "cache": planted["cache"], "agent-data": planted["agent-data"], "lock": weaveRunLifecycleLockPath(dir, 1)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived teardown: %s (%v)", kind, p, err)
		}
	}
	q, _ := loadWeaveQueue(dir)
	it := q.Items[0]
	if it.State != "abandoned" || it.Disposition != "rejected" || it.DispositionReason != "gate red twice" || it.SalvageRef != env.Data.PreservedRef || it.Workspace != "" || it.LogPath != "" || it.CtlSock != "" {
		t.Fatalf("row not compacted with its disposition: %+v", it)
	}
	// Idempotent: nothing left, nothing reported.
	if acts := weavePruneOwnedRun(dir, 1, "repo"); len(acts) != 0 {
		t.Fatalf("second pass acted: %+v", acts)
	}
}

func TestWeaveAbandonSupersededKeepsTheWord(t *testing.T) {
	root, workspace, _ := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "killed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "abandon", "1", "--yes", "--disposition", "superseded"); code != 0 || !strings.Contains(out, "abandoned as superseded") {
		t.Fatalf("exit=%d %s", code, out)
	}
	q, _ := loadWeaveQueue(dir)
	if q.Items[0].Disposition != "superseded" || q.Items[0].SalvageRef == "" {
		t.Fatalf("%+v", q.Items[0])
	}
}

func TestWeaveAbandonRejectsUnknownDisposition(t *testing.T) {
	root, workspace, _ := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{ID: 1, State: "failed", Workspace: workspace}}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"merged", "whatever"} {
		if _, code := runWeave(t, "abandon", "1", "--yes", "--disposition", d); code == 0 {
			t.Fatalf("--disposition %s accepted", d)
		}
	}
}

func TestWeavePruneOwnedRunRefusesUniqueWorkWithoutSalvage(t *testing.T) {
	root, workspace, _ := setupAbandonGuardFixture(t)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "failed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}
	if acts := weavePruneOwnedRun(dir, 1, "repo"); len(acts) != 0 {
		t.Fatalf("unique work was acted on: %+v", acts)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace with unique work gone: %v", err)
	}
	// Now an EMPTY failed run (no commits ahead): reclaimable, disposition empty.
	gitT(t, workspace, "reset", "-q", "--hard", "HEAD~1")
	acts := weavePruneOwnedRun(dir, 1, "repo")
	if len(acts) == 0 || acts[0].Kind != "workspace" || !acts[0].Done {
		t.Fatalf("empty run not reclaimed: %+v", acts)
	}
	q, _ := loadWeaveQueue(dir)
	if q.Items[0].Disposition != weaveDispositionEmpty {
		t.Fatalf("disposition = %q, want empty", q.Items[0].Disposition)
	}
}
