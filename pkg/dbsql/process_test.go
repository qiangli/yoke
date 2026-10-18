package dbsql

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
)

// The helpers below run as separate OS processes (the test binary re-exec'd),
// so every client in these tests is a genuinely independent process.

func TestSQLOwnerProcessHelper(t *testing.T) {
	if os.Getenv("DBSQL_OWNER_HELPER") != "1" {
		return
	}
	if err := dblifecycle.Serve(context.Background(), os.Getenv("DBSQL_READY"), 0); err != nil {
		t.Fatal(err)
	}
}

// TestSQLClientHelper sends one request. With DBSQL_DIRECT_PATH it instead
// runs DBSQL_REPEAT direct (owner-less) operations on a persistent file.
func TestSQLClientHelper(t *testing.T) {
	if os.Getenv("DBSQL_CLIENT_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := json.RawMessage(os.Getenv("DBSQL_INPUT"))
	if p := os.Getenv("DBSQL_DIRECT_PATH"); p != "" {
		n, _ := strconv.Atoi(os.Getenv("DBSQL_REPEAT"))
		target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
		for i := 0; i < n; i++ {
			if _, _, err := (dblifecycle.Service{}).RunCategory(ctx, target, os.Getenv("DBSQL_OP"), input, false, 10*time.Second); err != nil {
				fmt.Println(dbstore.Code(err), err)
				os.Exit(3)
			}
		}
		fmt.Println("ok")
		return
	}
	epoch, _ := strconv.ParseUint(os.Getenv("DBSQL_EPOCH"), 10, 64)
	var target dbstore.Target
	_ = json.Unmarshal([]byte(os.Getenv("DBSQL_TARGET")), &target)
	res, err := dblifecycle.Call(ctx, os.Getenv("DBSQL_SESSION"), dblifecycle.Request{
		Op: os.Getenv("DBSQL_OP"), Target: target, Input: input, Epoch: epoch, Yes: os.Getenv("DBSQL_YES") == "1",
		RequestID: os.Getenv("DBSQL_REQUEST_ID"), Wait: 5 * time.Second,
	})
	if err != nil {
		res = dblifecycle.Response{Error: &dblifecycle.OwnerError{Code: dbstore.Code(err), Message: err.Error()}}
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

type client struct {
	t       *testing.T
	session string
	target  dbstore.Target
}

type call struct {
	agent, op, requestID string
	in                   Input
	epoch                uint64
	yes                  bool
}

func (c client) cmd(r call) *exec.Cmd {
	in, _ := json.Marshal(r.in)
	tb, _ := json.Marshal(c.target)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSQLClientHelper$")
	yes := "0"
	if r.yes {
		yes = "1"
	}
	cmd.Env = append(os.Environ(), "DBSQL_CLIENT_HELPER=1", "DBSQL_SESSION="+c.session, "DBSQL_TARGET="+string(tb),
		"DBSQL_OP="+r.op, "DBSQL_INPUT="+string(in), "DBSQL_EPOCH="+strconv.FormatUint(r.epoch, 10), "DBSQL_YES="+yes,
		"DBSQL_REQUEST_ID="+r.requestID, "BASHY_EPISODE="+r.agent)
	return cmd
}

func (c client) do(r call) dblifecycle.Response {
	c.t.Helper()
	b, err := c.cmd(r).Output()
	if err != nil {
		c.t.Fatalf("client %s %s: %v %s", r.agent, r.op, err, b)
	}
	var res dblifecycle.Response
	if err := json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &res); err != nil {
		c.t.Fatalf("client output %q: %v", b, err)
	}
	return res
}

func (c client) ok(r call) dblifecycle.Response {
	c.t.Helper()
	res := c.do(r)
	if !res.OK {
		c.t.Fatalf("%s %s: %+v", r.agent, r.op, res.Error)
	}
	return res
}

func (c client) code(r call) string {
	c.t.Helper()
	res := c.do(r)
	if res.Error == nil {
		return ""
	}
	return res.Error.Code
}

func (c client) count(agent, table string) string {
	c.t.Helper()
	res := c.ok(call{agent: agent, op: OpQuery, in: Input{SQL: "select count(*) from " + table}})
	b, _ := json.Marshal(res.Result.(map[string]any)["rows"])
	return string(b)
}

func startSQLOwner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSQLOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "DBSQL_OWNER_HELPER=1", "DBSQL_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			return string(b)
		}
	}
	t.Fatal("owner did not become ready")
	return ""
}

func TestIndependentClientsShareOwnerMemoryDatabase(t *testing.T) {
	session := startSQLOwner(t)
	c := client{t: t, session: session, target: dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: session, Name: "shared"}}
	c.ok(call{agent: "agent-a", op: "create"})
	if code := c.code(call{agent: "agent-a", op: OpSchemaApply, in: Input{SQL: "create table n(id integer primary key, who text)"}}); code != "confirmation_required" {
		t.Fatalf("unconfirmed apply = %s", code)
	}
	c.ok(call{agent: "agent-a", op: OpSchemaApply, yes: true, in: Input{SQL: "create table n(id integer primary key, who text)"}})

	// Two writer processes and a reader process run at once.
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for _, agent := range []string{"agent-a", "agent-b"} {
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(agent string, i int) {
				defer wg.Done()
				res := c.do(call{agent: agent, op: OpExec, requestID: fmt.Sprintf("%s-%d", agent, i), in: Input{Statements: []Statement{
					{SQL: "insert into n(who) values (?)", Params: json.RawMessage(`["` + agent + `"]`)},
					{SQL: "insert into n(who) values (?)", Params: json.RawMessage(`["` + agent + `"]`)},
				}}})
				if !res.OK {
					errs <- fmt.Sprintf("%s %d: %+v", agent, i, res.Error)
				}
			}(agent, i)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if res := c.do(call{agent: "agent-c", op: OpQuery, in: Input{SQL: "select count(*) from n"}}); !res.OK {
			errs <- fmt.Sprintf("reader: %+v", res.Error)
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if got := c.count("agent-c", "n"); got != `[[{"t":"int","v":"20"}]]` {
		t.Fatalf("after concurrent writes: %s", got)
	}
	// A timed-out client may retry with its request id: nothing applies twice.
	replay := call{agent: "agent-a", op: OpExec, requestID: "agent-a-0", in: Input{Statements: []Statement{
		{SQL: "insert into n(who) values (?)", Params: json.RawMessage(`["agent-a"]`)},
		{SQL: "insert into n(who) values (?)", Params: json.RawMessage(`["agent-a"]`)},
	}}}
	c.ok(replay)
	if got := c.count("agent-c", "n"); got != `[[{"t":"int","v":"20"}]]` {
		t.Fatalf("replay applied again: %s", got)
	}
	reused := replay
	reused.in.Statements = reused.in.Statements[:1]
	if code := c.code(reused); code != "conflict" {
		t.Fatalf("request id reused with other input = %s", code)
	}

	// Exclusive hold: agent-a holds; agent-b is refused; a stale or missing
	// epoch is refused; the holder's read path still refuses mutation.
	hold := c.ok(call{agent: "agent-a", op: "hold"})
	epoch := uint64(hold.Result.(map[string]any)["epoch"].(float64))
	if code := c.code(call{agent: "agent-b", op: OpQuery, in: Input{SQL: "select 1"}}); code != "claim_held" {
		t.Fatalf("other agent during hold = %s", code)
	}
	if code := c.code(call{agent: "agent-b", op: OpExec, in: Input{SQL: "insert into n(who) values ('b')"}}); code != "claim_held" {
		t.Fatalf("other agent write during hold = %s", code)
	}
	if code := c.code(call{agent: "agent-a", op: OpQuery, in: Input{SQL: "select 1"}}); code != "stale_epoch" {
		t.Fatalf("holder without epoch = %s", code)
	}
	if code := c.code(call{agent: "agent-a", op: OpQuery, epoch: epoch + 1, in: Input{SQL: "select 1"}}); code != "stale_epoch" {
		t.Fatalf("holder with wrong epoch = %s", code)
	}
	if code := c.code(call{agent: "agent-a", op: OpQuery, epoch: epoch, in: Input{SQL: "pragma query_only=0; delete from n"}}); code != "read_only" {
		t.Fatalf("held read path mutation = %s", code)
	}
	c.ok(call{agent: "agent-a", op: OpSchemaApply, epoch: epoch, yes: true, in: Input{SQL: "alter table n add column note text"}})
	c.ok(call{agent: "agent-a", op: OpExec, epoch: epoch, in: Input{SQL: "insert into n(who, note) values ('a', 'held')"}})
	c.ok(call{agent: "agent-a", op: "release", epoch: epoch})
	if got := c.count("agent-b", "n"); got != `[[{"t":"int","v":"21"}]]` {
		t.Fatalf("after release: %s", got)
	}

	// Memory -> persistent through the owner and a direct import.
	out := filepath.Join(t.TempDir(), "mem-export")
	c.ok(call{agent: "agent-b", op: OpExport, in: Input{Out: out}})
	dst := filepath.Join(t.TempDir(), "copy.db")
	pt := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: dst}
	if _, err := (dblifecycle.Service{}).Create(context.Background(), pt, time.Second); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(Input{In: out, Replace: true})
	if _, _, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpImport, in, true, time.Second); err != nil {
		t.Fatal(err)
	}
	q, _ := json.Marshal(Input{SQL: "select count(*), count(note) from n"})
	_, res, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpQuery, q, false, time.Second)
	if b, _ := json.Marshal(res.Result.(map[string]any)["rows"]); err != nil || string(b) != `[[{"t":"int","v":"21"},{"t":"int","v":"1"}]]` {
		t.Fatalf("persistent copy: %s %v", b, err)
	}

	// The owner ends; its memory database is explicitly gone.
	c.ok(call{agent: "agent-a", op: "stop", yes: true})
	time.Sleep(100 * time.Millisecond)
	if code := c.code(call{agent: "agent-b", op: OpQuery, in: Input{SQL: "select 1"}}); code != "lifetime_ended" {
		t.Fatalf("after stop = %s", code)
	}
}

func TestIndependentProcessesSharePersistentFileDirectly(t *testing.T) {
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	p := filepath.Join(t.TempDir(), "direct.db")
	pt := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
	if _, err := (dblifecycle.Service{}).Create(context.Background(), pt, time.Second); err != nil {
		t.Fatal(err)
	}
	apply, _ := json.Marshal(Input{SQL: "create table w(id integer primary key, who text)"})
	if _, _, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpSchemaApply, apply, true, time.Second); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([]string, 3)
	for i, spec := range []struct{ op, input string }{
		{OpExec, `{"sql":"insert into w(who) values ('a')"}`},
		{OpExec, `{"sql":"insert into w(who) values ('b')"}`},
		{OpQuery, `{"sql":"select count(*) from w"}`},
	} {
		wg.Add(1)
		go func(i int, op, input string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLClientHelper$")
			cmd.Env = append(os.Environ(), "DBSQL_CLIENT_HELPER=1", "DBSQL_DIRECT_PATH="+p, "DBSQL_REPEAT=25", "DBSQL_OP="+op, "DBSQL_INPUT="+input)
			b, _ := cmd.CombinedOutput()
			outs[i] = string(b)
		}(i, spec.op, spec.input)
	}
	wg.Wait()
	for i, o := range outs {
		if !strings.HasPrefix(o, "ok") {
			t.Errorf("process %d: %s", i, o)
		}
	}
	q, _ := json.Marshal(Input{SQL: "select count(*) from w"})
	_, res, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpQuery, q, false, time.Second)
	if b, _ := json.Marshal(res.Result.(map[string]any)["rows"]); err != nil || string(b) != `[[{"t":"int","v":"50"}]]` {
		t.Fatalf("no lost writes: %s %v", b, err)
	}
}
