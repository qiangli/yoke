package lenscmd

import (
	"encoding/json"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/ref"
)

type runEvidenceResolver struct{ sources ref.RunSources }

func (r runEvidenceResolver) Resolve(id string) (ref.Node, error) {
	return ref.NewNode(ref.Run, id), nil
}
func (r runEvidenceResolver) RunSources(string) (ref.RunSources, error) { return r.sources, nil }

func TestRunListingHistoryCLI(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "work"), []byte("base"), 0600)
	wt, _ := repo.Worktree()
	wt.Add("work")
	base, err := wt.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now().Add(-time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "work"), []byte("delivery"), 0600)
	wt.Add("work")
	_, err = wt.Commit("delivery", &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now().Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "worker.log")
	os.WriteFile(log, []byte("test passed\n"), 0600)
	old := AgentWorldRefs
	AgentWorldRefs = ref.NewRegistry()
	t.Cleanup(func() { AgentWorldRefs = old })
	RegisterEntityResolver("run", runEvidenceResolver{ref.RunSources{Workspace: dir, BaseCommit: base.String(), StartedAt: time.Now().Add(-5 * time.Minute), LogPath: log}})
	out, stderr, code := runTool(t, dir, "", "-t", "listing", "run:test-1", "--since=10m")
	if code != 0 {
		t.Fatalf("listing: %d %s", code, stderr)
	}
	data, cursor := deltaCursor(t, out)
	for _, s := range []string{`"type":"file"`, `"type":"commit"`, `"type":"log-line"`} {
		if !strings.Contains(data, s) {
			t.Fatalf("missing %s: %s", s, out)
		}
	}
	out, stderr, code = runTool(t, dir, "", "-t", "listing", "run:test-1", "--since", cursor)
	if code != 0 {
		t.Fatalf("resume: %s", stderr)
	}
	data, _ = deltaCursor(t, out)
	if data != "" {
		t.Fatalf("unchanged: %s", data)
	}
	for _, args := range [][]string{{"-t", "listing", "run:test-2", "--since", cursor}, {"-t", "jsonl", "run:test-1", "--since", cursor}} {
		out, stderr, code = runTool(t, dir, "", args...)
		if code == 0 || out != "" {
			t.Fatalf("binding accepted: %d %s %s", code, out, stderr)
		}
	}
	os.Rename(log, log+".old")
	os.WriteFile(log, []byte("new log\n"), 0600)
	out, stderr, code = runTool(t, dir, "", "-t", "listing", "run:test-1", "--since", cursor)
	if code == 0 || out != "" || !strings.Contains(stderr, "restart") {
		t.Fatalf("rotation: %d %s %s", code, out, stderr)
	}
	out, stderr, code = runTool(t, dir, "", "-t", "listing", "run:test-1", "--since=start")
	if code != 0 || !strings.Contains(out, "new log") {
		t.Fatalf("restart: %d %s %s", code, out, stderr)
	}
}

func TestRunHistoryExpiredEnvelope(t *testing.T) {
	c := stream.Cursor{SchemaVersion: stream.CursorSchema, Kind: stream.KindChangeStream, Token: "opaque", Expiry: time.Now().Add(-time.Hour).Format(time.RFC3339), Binding: stream.Binding{Provider: "run", Target: "run:test-1", Query: "delta:listing"}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, code := runTool(t, t.TempDir(), "", "-t", "listing", "run:test-1", "--since", string(b))
	if code == 0 || out != "" || !strings.Contains(stderr, "expired") || !strings.Contains(stderr, "restart") {
		t.Fatalf("expired: %d %s %s", code, out, stderr)
	}
}
