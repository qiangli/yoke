package dbvec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// ownerCall is one owner request from a named agent.
type ownerCall struct {
	t       *testing.T
	session string
	target  dbstore.Target
}

func (o ownerCall) do(agent, op string, in Input, epoch uint64, yes bool, requestID string) dblifecycle.Response {
	o.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var b json.RawMessage
	if strings.HasPrefix(op, "vec.") {
		b, _ = json.Marshal(in)
	}
	holder := coord.Self()
	holder.Episode = agent
	res, err := dblifecycle.Call(ctx, o.session, dblifecycle.Request{Op: op, Target: o.target, Input: b, Epoch: epoch, Yes: yes,
		RequestID: requestID, Holder: holder, Wait: 5 * time.Second})
	if err != nil {
		return dblifecycle.Response{Error: &dblifecycle.OwnerError{Code: dbstore.Code(err), Message: err.Error()}}
	}
	return res
}

func (o ownerCall) ok(agent, op string, in Input, epoch uint64, yes bool, requestID string) dblifecycle.Response {
	o.t.Helper()
	res := o.do(agent, op, in, epoch, yes, requestID)
	if !res.OK {
		o.t.Fatalf("%s %s: %+v", agent, op, res.Error)
	}
	return res
}

func (o ownerCall) code(agent, op string, in Input, epoch uint64) string {
	o.t.Helper()
	if res := o.do(agent, op, in, epoch, false, ""); res.Error != nil {
		return res.Error.Code
	}
	return ""
}

// startOwner runs the explicit owner in this process: its memory
// databases live in the owner's MemSpace and are reached only by Call.
func startOwner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	ready := filepath.Join(dir, "ready")
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- dblifecycle.Serve(ctx, ready, 0) }()
	t.Cleanup(func() { stop(); <-served })
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			return string(b)
		}
	}
	t.Fatal("owner did not become ready")
	return ""
}

func epochOf(t *testing.T, res dblifecycle.Response) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(string(res.Result.(map[string]any)["epoch"].(json.Number)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func hitIDs(res dblifecycle.Response) string {
	var ids []string
	for _, h := range res.Result.(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, ",")
}

// Two agents share one owner-held memory collection: writes, revision
// conflicts, request-id replay, an exclusive hold with epoch fencing, an
// export to a persistent copy and the owner's declared end.
func TestOwnerSessionSharesMemoryCollection(t *testing.T) {
	session := startOwner(t)
	o := ownerCall{t: t, session: session, target: dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: session, Name: "shared"}}
	o.ok("agent-a", "create", Input{}, 0, false, "")
	if c := o.do("agent-a", OpCollectionAdd, Input{Collection: "e", Dimension: 2, Metric: "l2", Model: "m@1"}, 0, false, ""); c.Error == nil || c.Error.Code != "confirmation_required" {
		t.Fatalf("unconfirmed add: %+v", c.Error)
	}
	o.ok("agent-a", OpCollectionAdd, Input{Collection: "e", Dimension: 2, Metric: "l2", Model: "m@1"}, 0, true, "")
	o.ok("agent-a", OpPut, Input{Collection: "e", ID: "a", Vector: json.RawMessage(`[1,0]`), Metadata: json.RawMessage(`{"by":"a"}`)}, 0, false, "")
	o.ok("agent-b", OpPut, Input{Collection: "e", ID: "b", Vector: tagged(lossy[:2]), Metadata: json.RawMessage(`{"by":"b"}`)}, 0, false, "")
	if got := hitIDs(o.ok("agent-b", OpSearch, Input{Collection: "e", Vector: json.RawMessage(`[1,0]`), Filter: json.RawMessage(`{"field":"by","op":"eq","value":"a"}`)}, 0, false, "")); got != "a" {
		t.Fatalf("agent-b sees agent-a's vector: %s", got)
	}
	// A contested update is a conflict, not a lost write.
	o.ok("agent-a", OpPut, Input{Collection: "e", ID: "a", IfRevision: rev(1), Vector: json.RawMessage(`[2,0]`)}, 0, false, "")
	if c := o.code("agent-b", OpPut, Input{Collection: "e", ID: "a", IfRevision: rev(1), Vector: json.RawMessage(`[3,0]`)}, 0); c != "conflict" {
		t.Fatalf("stale writer: %s", c)
	}
	// A retried request id applies once; reusing it with other input conflicts.
	put := Input{Collection: "e", ID: "r", Vector: json.RawMessage(`[0,1]`)}
	first := o.ok("agent-a", OpPut, put, 0, false, "req-1")
	again := o.ok("agent-a", OpPut, put, 0, false, "req-1")
	if first.Result.(map[string]any)["revision"].(json.Number) != "1" || again.Result.(map[string]any)["revision"].(json.Number) != "1" {
		t.Fatalf("replay applied twice: %v %v", first.Result, again.Result)
	}
	put.Vector = json.RawMessage(`[1,1]`)
	if res := o.do("agent-a", OpPut, put, 0, false, "req-1"); res.Error == nil || res.Error.Code != "conflict" {
		t.Fatalf("request id reused with other input: %+v", res.Error)
	}
	// Exclusive hold: the other agent is refused reads and writes; the
	// holder needs its current epoch.
	epoch := epochOf(t, o.ok("agent-a", "hold", Input{}, 0, false, ""))
	if c := o.code("agent-b", OpSearch, Input{Collection: "e", Vector: json.RawMessage(`[1,0]`)}, 0); c != "claim_held" {
		t.Fatalf("other agent search during hold: %s", c)
	}
	if c := o.code("agent-b", OpPut, Input{Collection: "e", ID: "x", Vector: json.RawMessage(`[1,0]`)}, 0); c != "claim_held" {
		t.Fatalf("other agent put during hold: %s", c)
	}
	if c := o.code("agent-a", OpGet, Input{Collection: "e", ID: "a"}, epoch+1); c != "stale_epoch" {
		t.Fatalf("holder with wrong epoch: %s", c)
	}
	o.ok("agent-a", OpCollectionAdd, Input{Collection: "held", Dimension: 1, Metric: "l1", Model: "m@1"}, epoch, true, "")
	o.ok("agent-a", "release", Input{}, epoch, false, "")
	// Memory -> persistent copy through the owner's consistent export.
	out := filepath.Join(t.TempDir(), "mem-export")
	o.ok("agent-b", OpExport, Input{Out: out}, 0, false, "")
	pt := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "copy.db")}
	if _, err := (dblifecycle.Service{}).Create(context.Background(), pt, time.Second); err != nil {
		t.Fatal(err)
	}
	p := &fixture{t: t, target: pt}
	p.must(OpImport, Input{In: out, Replace: true}, true)
	if it := p.get("e", "b"); !sameBits(decoded(t, it.Vector), lossy[:2]) || string(it.Metadata) != `{"by":"b"}` {
		t.Fatalf("persistent copy: %+v", it)
	}
	if p.get("e", "a").Revision != 2 {
		t.Fatal("revision not preserved")
	}
	// The owner ends; its memory collection is explicitly gone.
	o.ok("agent-a", "stop", Input{}, 0, true, "")
	time.Sleep(100 * time.Millisecond)
	if c := o.code("agent-b", OpGet, Input{Collection: "e", ID: "a"}, 0); c != "lifetime_ended" {
		t.Fatalf("after stop: %s", c)
	}
}
