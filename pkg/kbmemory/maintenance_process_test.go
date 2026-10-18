package kbmemory

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaintainProcessHelper is the child: it runs one maintenance verb named
// by the environment and prints the envelope as JSON.
func TestMaintainProcessHelper(t *testing.T) {
	if os.Getenv("KB_MAINTAIN_HELPER") != "1" {
		t.Skip("helper only")
	}
	var req MaintainRequest
	if err := json.Unmarshal([]byte(os.Getenv("KB_MAINTAIN_REQ")), &req); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(Maintain(context.Background(), req)); err != nil {
		t.Fatal(err)
	}
}

func childMaintain(t *testing.T, req MaintainRequest) Envelope {
	t.Helper()
	b, _ := json.Marshal(req)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMaintainProcessHelper$")
	cmd.Env = append(os.Environ(), "KB_MAINTAIN_HELPER=1", "KB_MAINTAIN_REQ="+string(b))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child maintain %s: %v", req.Verb, err)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(strings.SplitN(string(out), "\n", 2)[0]), &env); err != nil {
		t.Fatalf("child maintain output: %v in %q", err, out)
	}
	return env
}

// maintainCross: the parent captures and indexes, a separate process deletes
// one record with index reconciliation, and the parent then sees the deletion
// in both the source and the rank index.
func maintainCross(t *testing.T, dir, doc, idx string) {
	t.Helper()
	must(t, Request{Verb: "init", Target: doc})
	mkIndex(t, idx, "")
	keep := addBody(t, doc, "", "durable wheat note", nil)
	gone := addBody(t, doc, "", "doomed wheat note", nil)
	if env := SyncIndex(context.Background(), SyncQuery{Target: doc, Index: idx}); !env.OK {
		t.Fatalf("sync: %+v", env.Error)
	}

	env := childMaintain(t, MaintainRequest{Verb: "delete", Target: doc, Index: idx, Dir: dir, ID: gone.ID,
		IfRevision: revOf(t, doc, "", gone.ID), Yes: true})
	if !env.OK {
		t.Fatalf("child delete: %+v", env.Error)
	}
	if code := maintainCode(t, MaintainRequest{Verb: "delete", Target: doc, ID: gone.ID, IfRevision: 1, Yes: true}); code == "" {
		t.Fatal("deleted record still deletable")
	}
	rep := Retrieve(context.Background(), RetrieveQuery{Target: doc, Index: idx, Limit: 10, Text: "wheat"})
	if !rep.OK {
		t.Fatalf("recall: %+v", rep.Error)
	}
	b, _ := json.Marshal(rep.Result)
	if strings.Contains(string(b), gone.ID) || !strings.Contains(string(b), keep.ID) {
		t.Fatalf("recall after child delete = %s", b)
	}
}

func TestMaintainTwoProcessesPersistent(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process handoff runs on the remote manager gate, not under -short")
	}
	dir := isolate(t)
	maintainCross(t, dir, loc("sqlite:file:"+filepath.Join(dir, "mem.db")), loc("bleve:file:"+filepath.Join(dir, "mem.bleve")))
}

func TestMaintainTwoProcessesMemoryOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process handoff runs on the remote manager gate, not under -short")
	}
	dir := isolate(t)
	s := owner(t, dir)
	maintainCross(t, dir, loc("sqlite:mem:"+s+"/scratch"), loc("bleve:mem:"+s+"/rank"))
}
