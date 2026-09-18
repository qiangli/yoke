package graphcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

// contribRepo makes a temp dir that looks like a repo root (has .git) so the store
// resolves there.
func contribRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func contribEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	return []string{
		"BASHY_AGENT_ID=tester",
		"BASHY_EPISODE=ep1",
		"BASHY_KB_DIR=" + filepath.Join(root, "kb"),
		"BASHY_HOME=" + filepath.Join(root, "home"),
		"BASHY_SKILLS_DIR=" + filepath.Join(root, "skills"),
		"YCODE_DATA_DIR=" + filepath.Join(root, "ycode"),
	}
}

func runc(t *testing.T, dir string, fn func(*tool.RunContext, []string) int, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Env:   contribEnv(t),
		FS:    tool.NewLocalFS(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &o, Err: &e},
	}
	code = fn(rc, args)
	return o.String(), e.String(), code
}

func TestNoteAppendAndRecall(t *testing.T) {
	dir := contribRepo(t)
	if _, e, code := runc(t, dir, runNote, "internal/tunnel", "handshake is HMAC-signed", "--plain"); code != 0 {
		t.Fatalf("note failed: %d %s", code, e)
	}
	// The store file exists at the repo-local path.
	if _, err := os.Stat(filepath.Join(dir, "docs", "kb", contribRel)); err != nil {
		t.Fatalf("store not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "bashy", "graph", "contrib.jsonl")); err == nil {
		t.Fatal("graph note wrote the legacy .agents contribution log")
	}
	out, _, code := runc(t, dir, runRecall, "handshake", "--json")
	if code != 0 {
		t.Fatal("recall failed")
	}
	var env contribEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if env.Count != 1 || env.Contributions[0].Text != "handshake is HMAC-signed" {
		t.Fatalf("recall miss: %+v", env)
	}
	if env.Contributions[0].By != "tester" || env.Contributions[0].Episode != "ep1" {
		t.Errorf("provenance not captured: %+v", env.Contributions[0])
	}
	if env.Contributions[0].TargetID == "" || len(env.Contributions[0].TargetID) != 16 {
		t.Fatalf("entity id not stamped: %+v", env.Contributions[0])
	}
}

func TestNoteIdempotent(t *testing.T) {
	dir := contribRepo(t)
	runc(t, dir, runNote, "x", "same fact", "--plain")
	runc(t, dir, runNote, "x", "same fact", "--plain")
	out, _, _ := runc(t, dir, runRecall, "--target", "x", "--json")
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if env.Count != 1 {
		t.Fatalf("re-noting the same fact should be idempotent, got %d", env.Count)
	}
}

func TestLinkAndNotesFor(t *testing.T) {
	dir := contribRepo(t)
	runc(t, dir, runLink, "outpost", "dials", "cloudbox", "--plain")
	out, _, code := runc(t, dir, runNotesFor, "cloudbox", "--json")
	if code != 0 {
		t.Fatal("notes failed")
	}
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	// notes-for a target includes links pointing TO it (Dst match).
	if env.Count != 1 || env.Contributions[0].Relation != "dials" {
		t.Fatalf("link not found via notes-for dst: %+v", env)
	}
	if env.Contributions[0].TargetID == "" || env.Contributions[0].DstID == "" {
		t.Fatalf("link entity ids not stamped: %+v", env.Contributions[0])
	}
}

func TestEntityIDsStableAcrossRuns(t *testing.T) {
	dir := contribRepo(t)
	runc(t, dir, runLink, "kb:alpha", "about", "todo:123", "--plain")
	out1, _, _ := runc(t, dir, runNotesFor, "todo:123", "--json")
	runc(t, dir, runLink, "kb:alpha", "about", "todo:123", "--plain")
	out2, _, _ := runc(t, dir, runNotesFor, "todo:123", "--json")
	var env1, env2 contribEnvelope
	_ = json.Unmarshal([]byte(out1), &env1)
	_ = json.Unmarshal([]byte(out2), &env2)
	if env1.Count != 1 || env2.Count != 1 {
		t.Fatalf("link should remain idempotent across runs: %d %d", env1.Count, env2.Count)
	}
	a, b := env1.Contributions[0], env2.Contributions[0]
	if a.ID != b.ID || a.TargetID != b.TargetID || a.DstID != b.DstID {
		t.Fatalf("ids changed across runs:\n%+v\n%+v", a, b)
	}
}

func TestObserveAndPitfalls(t *testing.T) {
	dir := contribRepo(t)
	runc(t, dir, runObserve, "test", "cloudbox-hub", "--outcome", "failure", "--summary", "k3s cgroups darwin", "--plain")
	runc(t, dir, runObserve, "test", "cloudbox-hub", "--outcome", "success", "--summary", "make test in docker", "--plain")
	out, _, code := runc(t, dir, runPitfalls, "cloudbox-hub", "--json")
	if code != 0 {
		t.Fatal("pitfalls failed")
	}
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if env.Count != 1 || env.Contributions[0].Outcome != "failure" {
		t.Fatalf("pitfalls should return only the failure: %+v", env)
	}
	if !strings.Contains(env.Contributions[0].Text, "cgroups") {
		t.Errorf("failure summary lost: %+v", env.Contributions[0])
	}
}

func TestForgetById(t *testing.T) {
	dir := contribRepo(t)
	out, _, _ := runc(t, dir, runNote, "y", "temporary", "--json")
	var a ackEnvelope
	_ = json.Unmarshal([]byte(out), &a)
	if a.ID == "" {
		t.Fatal("no id in ack")
	}
	if _, _, code := runc(t, dir, runForget, a.ID, "--plain"); code != 0 {
		t.Fatal("forget failed")
	}
	out2, _, _ := runc(t, dir, runRecall, "--target", "y", "--json")
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out2), &env)
	if env.Count != 0 {
		t.Fatalf("forgotten note should not recall, got %d", env.Count)
	}
}

func TestForgetByTarget(t *testing.T) {
	dir := contribRepo(t)
	runc(t, dir, runNote, "z", "fact one", "--plain")
	runc(t, dir, runNote, "z", "fact two", "--plain")
	runc(t, dir, runForget, "--target", "z", "--plain")
	out, _, _ := runc(t, dir, runRecall, "--target", "z", "--json")
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if env.Count != 0 {
		t.Fatalf("--target forget should drop all of z, got %d", env.Count)
	}
}

func TestSharedAcrossSubdirs(t *testing.T) {
	dir := contribRepo(t)
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write from a subdir; the store must resolve to the repo root.
	runc(t, sub, runNote, "shared", "from a subdir", "--plain")
	// Read from the root: same store.
	out, _, _ := runc(t, dir, runRecall, "--target", "shared", "--json")
	var env contribEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if env.Count != 1 {
		t.Fatalf("subdir contribution not shared to repo root store: %d", env.Count)
	}
	// And the store lives at the root, not the subdir.
	if _, err := os.Stat(filepath.Join(dir, "docs", "kb", contribRel)); err != nil {
		t.Fatalf("store should be at repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "docs", "kb", contribRel)); err == nil {
		t.Fatal("store should NOT be created in the subdir")
	}
}

func TestConcurrentAppendsAllLand(t *testing.T) {
	dir := contribRepo(t)
	st := openStore(filepath.Join(dir, "docs", "kb"))
	done := make(chan int, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			c := Contribution{ID: contribID("note", "c", string(rune('a'+n))), Op: "note", Target: "c", Text: string(rune('a' + n))}
			_ = st.append(c)
			done <- n
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	all, err := st.all()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Fatalf("concurrent appends lost records: got %d want 10", len(all))
	}
}
