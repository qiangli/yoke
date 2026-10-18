package dbvec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// The helpers below run as separate OS processes (the test binary
// re-exec'd), so every client in these tests is a genuinely independent
// process. The tests that start them skip under -short.

func TestVecOwnerProcessHelper(t *testing.T) {
	if os.Getenv("DBVEC_OWNER_HELPER") != "1" {
		return
	}
	if err := dblifecycle.Serve(context.Background(), os.Getenv("DBVEC_READY"), 0); err != nil {
		t.Fatal(err)
	}
}

// TestVecClientHelper runs DBVEC_REPEAT operations, each with DBVEC_INPUT
// where %d is the iteration, through the owner DBVEC_SESSION or directly on
// the persistent DBVEC_DIRECT_PATH, and prints the last response.
func TestVecClientHelper(t *testing.T) {
	if os.Getenv("DBVEC_CLIENT_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, _ := strconv.Atoi(os.Getenv("DBVEC_REPEAT"))
	var res dblifecycle.Response
	for i := 0; i < max(n, 1); i++ {
		input := json.RawMessage(strings.ReplaceAll(os.Getenv("DBVEC_INPUT"), "%d", strconv.Itoa(i)))
		if p := os.Getenv("DBVEC_DIRECT_PATH"); p != "" {
			target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
			_, out, err := (dblifecycle.Service{}).RunCategory(ctx, target, os.Getenv("DBVEC_OP"), input, os.Getenv("DBVEC_YES") == "1", 10*time.Second)
			res = dblifecycle.Response{OK: err == nil, Result: out.Result}
			if err != nil {
				res.Error = &dblifecycle.OwnerError{Code: dbstore.Code(err), Message: err.Error()}
			}
		} else {
			var target dbstore.Target
			_ = json.Unmarshal([]byte(os.Getenv("DBVEC_TARGET")), &target)
			var err error
			res, err = dblifecycle.Call(ctx, os.Getenv("DBVEC_SESSION"), dblifecycle.Request{
				Op: os.Getenv("DBVEC_OP"), Target: target, Input: input, Yes: os.Getenv("DBVEC_YES") == "1", Wait: 10 * time.Second,
			})
			if err != nil {
				res = dblifecycle.Response{Error: &dblifecycle.OwnerError{Code: dbstore.Code(err), Message: err.Error()}}
			}
		}
		if !res.OK {
			break
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

type proc struct {
	agent, op, input, session, direct string
	target                            dbstore.Target
	repeat                            int
	yes                               bool
}

func (p proc) run(t *testing.T) dblifecycle.Response {
	t.Helper()
	tb, _ := json.Marshal(p.target)
	cmd := exec.Command(os.Args[0], "-test.run=^TestVecClientHelper$")
	yes := "0"
	if p.yes {
		yes = "1"
	}
	cmd.Env = append(os.Environ(), "DBVEC_CLIENT_HELPER=1", "DBVEC_SESSION="+p.session, "DBVEC_TARGET="+string(tb), "DBVEC_DIRECT_PATH="+p.direct,
		"DBVEC_OP="+p.op, "DBVEC_INPUT="+p.input, "DBVEC_REPEAT="+strconv.Itoa(p.repeat), "DBVEC_YES="+yes, "BASHY_EPISODE="+p.agent)
	b, err := cmd.Output()
	var res dblifecycle.Response
	dec := json.NewDecoder(strings.NewReader(strings.SplitN(string(b), "\n", 2)[0]))
	dec.UseNumber()
	if derr := dec.Decode(&res); derr != nil {
		t.Errorf("client %s %s: %v %v %q", p.agent, p.op, err, derr, b)
	}
	return res
}

func (p proc) code(t *testing.T) string {
	t.Helper()
	if res := p.run(t); res.Error != nil {
		return res.Error.Code
	}
	return ""
}

// concurrently runs every proc at once and reports any failure.
func concurrently(t *testing.T, ps ...proc) {
	t.Helper()
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func(p proc) {
			defer wg.Done()
			if res := p.run(t); !res.OK {
				t.Errorf("%s %s: %+v", p.agent, p.op, res.Error)
			}
		}(p)
	}
	wg.Wait()
}

func startOwnerProcess(t *testing.T) (string, *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestVecOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "DBVEC_OWNER_HELPER=1", "DBVEC_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			return string(b), cmd
		}
	}
	t.Fatal("owner did not become ready")
	return "", nil
}

const putInput = `{"collection":"e","id":"%s-%%d","vector":[%%d,1],"metadata":{"by":"%s"}}`

// Independent client processes share one owner-held memory collection;
// the owner crashing ends it explicitly.
func TestIndependentProcessesShareOwnerMemoryCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("starts independent processes")
	}
	session, owner := startOwnerProcess(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: session, Name: "shared"}
	base := proc{session: session, target: target}
	with := func(agent, op, input string, repeat int, yes bool) proc {
		p := base
		p.agent, p.op, p.input, p.repeat, p.yes = agent, op, input, repeat, yes
		return p
	}
	if res := with("agent-a", "create", "", 1, false).run(t); !res.OK {
		t.Fatalf("create: %+v", res.Error)
	}
	if res := with("agent-a", OpCollectionAdd, `{"collection":"e","dimension":2,"metric":"l2","model":"m@1"}`, 1, true).run(t); !res.OK {
		t.Fatalf("add: %+v", res.Error)
	}
	search := `{"collection":"e","vector":[0,1],"filter":{"field":"by","op":"eq","value":"b"},"limit":100}`
	concurrently(t,
		with("agent-a", OpPut, fmt.Sprintf(putInput, "a", "a"), 15, false),
		with("agent-b", OpPut, fmt.Sprintf(putInput, "b", "b"), 15, false),
		with("agent-c", OpSearch, search, 10, false),
	)
	res := with("agent-c", OpSearch, search, 1, false).run(t)
	if hits := res.Result.(map[string]any)["hits"].([]any); len(hits) != 15 {
		t.Fatalf("agent-b vectors seen by agent-c: %d", len(hits))
	}
	// Same-id contention: only one if_absent insert wins.
	var wg sync.WaitGroup
	codes := make([]string, 2)
	for i, agent := range []string{"agent-a", "agent-b"} {
		wg.Add(1)
		go func(i int, agent string) {
			defer wg.Done()
			codes[i] = with(agent, OpPut, `{"collection":"e","id":"same","vector":[1,1],"if_absent":true}`, 1, false).code(t)
		}(i, agent)
	}
	wg.Wait()
	if !(codes[0] == "" && codes[1] == "exists" || codes[0] == "exists" && codes[1] == "") {
		t.Fatalf("same-id inserts: %v", codes)
	}
	// The owner crashes: the memory collection is gone, reported, not hung.
	_ = owner.Process.Kill()
	_ = owner.Wait()
	if c := with("agent-b", OpGet, `{"collection":"e","id":"a-0"}`, 1, false).code(t); c != "lifetime_ended" {
		t.Fatalf("after owner crash: %s", c)
	}
}

// Independent processes share a persistent file directly; it survives every
// writer's exit, and an exclusive op from another process is refused while
// the native exclusive lock is held.
func TestIndependentProcessesSharePersistentCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("starts independent processes")
	}
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	p := filepath.Join(t.TempDir(), "direct.db")
	f := &fixture{t: t, target: dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}}
	if _, err := f.svc.Create(context.Background(), f.target, time.Second); err != nil {
		t.Fatal(err)
	}
	f.add("e", 2, "l2")
	direct := func(agent, op, input string, repeat int, yes bool) proc {
		return proc{agent: agent, op: op, input: input, repeat: repeat, yes: yes, direct: p}
	}
	concurrently(t,
		direct("agent-a", OpPut, fmt.Sprintf(putInput, "a", "a"), 20, false),
		direct("agent-b", OpPut, fmt.Sprintf(putInput, "b", "b"), 20, false),
		direct("agent-c", OpSearch, `{"collection":"e","vector":[0,1]}`, 10, false),
	)
	list, _ := json.Marshal(f.must(OpCollectionList, Input{}, false).Result)
	if !strings.Contains(string(list), `"vectors":40`) {
		t.Fatalf("no lost writes across processes: %s", list)
	}
	// A deletion by one process is seen by the next one after it exits.
	if res := direct("agent-a", OpRm, `{"collection":"e","id":"a-0"}`, 1, false).run(t); !res.OK {
		t.Fatalf("rm: %+v", res.Error)
	}
	if c := direct("agent-b", OpGet, `{"collection":"e","id":"a-0"}`, 1, false).code(t); c != "not_found" {
		t.Fatalf("deleted vector in another process: %s", c)
	}
	h, err := (sqlitedb.Adapter{}).Open(context.Background(), f.target, dbstore.Options{Exclusive: true, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c := direct("agent-b", OpCollectionRm, `{"collection":"e"}`, 1, true).code(t); c != "busy" {
		t.Fatalf("exclusive op from another process under a held lock: %s", c)
	}
	h.Close()
	if f.get("e", "b-3").Revision != 1 {
		t.Fatal("refused exclusive op changed the collection")
	}
}
