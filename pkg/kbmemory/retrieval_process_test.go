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

// This file proves two genuinely independent OS processes share one memory
// collection and one rank index in both storage modes: the parent captures,
// a child syncs and recalls. The manager runs the whole package and the
// live CLI/MCP gates remotely after review.

// TestRetrieveProcessHelper is the child: it runs one retrieval verb named
// by the environment and prints the envelope as JSON. It never runs except
// as a spawned helper.
func TestRetrieveProcessHelper(t *testing.T) {
	if os.Getenv("KB_RETRIEVAL_HELPER") != "1" {
		t.Skip("helper only")
	}
	ctx := context.Background()
	target, index, session := os.Getenv("KB_RETRIEVAL_TARGET"), os.Getenv("KB_RETRIEVAL_INDEX"), os.Getenv("KB_RETRIEVAL_SESSION")
	var env Envelope
	switch os.Getenv("KB_RETRIEVAL_OP") {
	case "sync":
		env = SyncIndex(ctx, SyncQuery{Target: target, Index: index, Session: session})
	case "retrieve":
		env = Retrieve(ctx, RetrieveQuery{Target: target, Index: index, Session: session, Limit: 10, Text: os.Getenv("KB_RETRIEVAL_TEXT")})
	default:
		t.Fatalf("unknown KB_RETRIEVAL_OP %q", os.Getenv("KB_RETRIEVAL_OP"))
	}
	if err := json.NewEncoder(os.Stdout).Encode(env); err != nil {
		t.Fatal(err)
	}
}

func childVerb(t *testing.T, op, target, index, session, text string) Envelope {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRetrieveProcessHelper$")
	cmd.Env = append(os.Environ(),
		"KB_RETRIEVAL_HELPER=1", "KB_RETRIEVAL_OP="+op,
		"KB_RETRIEVAL_TARGET="+target, "KB_RETRIEVAL_INDEX="+index,
		"KB_RETRIEVAL_SESSION="+session, "KB_RETRIEVAL_TEXT="+text)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("child %s: %v", op, err)
	}
	var env Envelope
	// The envelope is the child's first line; the test binary's own PASS
	// follows it, the same shape the dblifecycle helper clients use.
	if err := json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &env); err != nil {
		t.Fatalf("child %s output: %v in %q", op, err, b)
	}
	return env
}

func syncResult(t *testing.T, env Envelope) SyncResult {
	t.Helper()
	if !env.OK || env.Op != SyncOp {
		t.Fatalf("child sync: %+v", env.Error)
	}
	var res SyncResult
	b, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("child sync result %s: %v", b, err)
	}
	return res
}

func retrieveResult(t *testing.T, env Envelope) RetrieveResult {
	t.Helper()
	if !env.OK || env.Op != RetrievalOp {
		t.Fatalf("child retrieve: %+v", env.Error)
	}
	var res RetrieveResult
	b, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("child retrieve result %s: %v", b, err)
	}
	return res
}

// crossProcess runs the two-process handoff: the parent captures, a child
// syncs, another child recalls, the parent adds once more, and a final child
// sees the stale count gap. It runs in both storage modes.
func crossProcess(t *testing.T, doc, idx, session string) {
	t.Helper()
	must(t, Request{Verb: "init", Target: doc, Session: session})
	mkIndex(t, idx, session)
	a := addBody(t, doc, session, "shared wheat note", nil)

	synced := syncResult(t, childVerb(t, "sync", doc, idx, session, ""))
	if synced.SourceDocuments != 1 || synced.Added != 1 || synced.IndexDocuments != 1 {
		t.Fatalf("child sync = %+v", synced)
	}
	res := retrieveResult(t, childVerb(t, "retrieve", doc, idx, session, "wheat"))
	if len(res.Hits) != 1 || res.Hits[0].Ref != refOf(t, doc, a.ID) || res.Freshness.Stale {
		t.Fatalf("child recall = %+v", res)
	}

	addBody(t, doc, session, "second wheat note, only the parent knows", nil)
	stale := retrieveResult(t, childVerb(t, "retrieve", doc, idx, session, ""))
	if f := stale.Freshness; !f.Stale || f.SourceDocuments != 2 || f.IndexDocuments != 1 {
		t.Fatalf("child recall after parent add = %+v", f)
	}
}

func TestRetrieveTwoProcessesPersistent(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process handoff runs on the remote manager gate, not under -short")
	}
	dir := isolate(t)
	crossProcess(t,
		loc("sqlite:file:"+filepath.Join(dir, "mem.db")),
		loc("bleve:file:"+filepath.Join(dir, "mem.bleve")), "")
}

func TestRetrieveTwoProcessesMemoryOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process handoff runs on the remote manager gate, not under -short")
	}
	dir := isolate(t)
	s := owner(t, dir)
	crossProcess(t, loc("sqlite:mem:"+s+"/scratch"), loc("bleve:mem:"+s+"/rank"), "")
}
