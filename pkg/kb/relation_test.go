package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateRelationStores(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_KB_DIR", filepath.Join(root, "host-kb"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "bashy-home"))
	t.Setenv("BASHY_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("YCODE_DATA_DIR", filepath.Join(root, "agent-data"))
	return root
}

func TestRelationRingReplayForgetMatchesContrib(t *testing.T) {
	root := isolateRelationStores(t)
	dir := filepath.Join(root, "repo", RepoSub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"n1","op":"note","target":"pkg/x","text":"keep me"}
{"id":"n2","op":"note","target":"pkg/y","text":"hide me"}
{"id":"f1","op":"forget","forget_id":"n2"}
`
	if err := os.WriteFile(RelationPath(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err := (RelationRing{Dir: dir}).Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "n1" {
		t.Fatalf("forget replay mismatch: %+v", live)
	}
}

func TestKBSearchFormRelationReadsGraphJSONL(t *testing.T) {
	root := isolateRelationStores(t)
	dir := filepath.Join(root, "kb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RelationPath(dir), []byte(`{"id":"r1","op":"link","target":"kb:alpha","relation":"about","dst":"todo:123"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, dir, "search", "--form", "relation", "alpha")
	if !strings.Contains(out, "kb:alpha") || !strings.Contains(out, "about") {
		t.Fatalf("relation search missed graph.jsonl:\n%s", out)
	}
}

func TestDoctorFlagsOpenVocabularyRelation(t *testing.T) {
	root := isolateRelationStores(t)
	dir := filepath.Join(root, "kb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RelationPath(dir), []byte(`{"id":"r1","op":"link","target":"a","relation":"dials","dst":"b"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, dir, "doctor")
	if !strings.Contains(out, "open-vocabulary relations") || !strings.Contains(out, "dials") {
		t.Fatalf("doctor did not flag open vocabulary relation:\n%s", out)
	}
}

func TestFederateReadsLegacyContribReadOnly(t *testing.T) {
	root := isolateRelationStores(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agents", "bashy", "graph"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LegacyRepoContribPath(repo), []byte(`{"id":"old1","op":"note","target":"legacy","text":"old bridge"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, filepath.Join(repo, RepoSub), "search", "--federate", "bridge")
	if !strings.Contains(out, "repo-graph") || !strings.Contains(out, "old bridge") {
		t.Fatalf("legacy contrib not federated:\n%s", out)
	}
	if _, err := os.Stat(RelationPath(filepath.Join(repo, RepoSub))); !os.IsNotExist(err) {
		t.Fatalf("federated legacy read created new relation log: %v", err)
	}
}
