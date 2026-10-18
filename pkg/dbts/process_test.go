package dbts

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

// Independent-process tests. They re-exec the test binary as owner and
// client processes, so every client is a genuinely independent process.
// They MUST skip under testing.Short: Dragon runs short in-process tests
// only, and the manager batches whole-package, process, live and remote
// gates.

func TestTSOwnerProcessHelper(t *testing.T) {
	if os.Getenv("DBTS_OWNER_HELPER") != "1" {
		return
	}
	if err := dblifecycle.Serve(context.Background(), os.Getenv("DBTS_READY"), 0); err != nil {
		t.Fatal(err)
	}
}

// TestTSClientHelper sends one request. With DBTS_DIRECT_PATH it instead
// runs DBTS_REPEAT direct (owner-less) operations on a persistent file.
func TestTSClientHelper(t *testing.T) {
	if os.Getenv("DBTS_CLIENT_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	input := json.RawMessage(os.Getenv("DBTS_INPUT"))
	if p := os.Getenv("DBTS_DIRECT_PATH"); p != "" {
		n, _ := strconv.Atoi(os.Getenv("DBTS_REPEAT"))
		target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
		for i := 0; i < n; i++ {
			if _, _, err := (dblifecycle.Service{}).RunCategory(ctx, target, os.Getenv("DBTS_OP"), input, false, 10*time.Second); err != nil {
				fmt.Println(dbstore.Code(err), err)
				os.Exit(3)
			}
		}
		fmt.Println("ok")
		return
	}
	epoch, _ := strconv.ParseUint(os.Getenv("DBTS_EPOCH"), 10, 64)
	var target dbstore.Target
	_ = json.Unmarshal([]byte(os.Getenv("DBTS_TARGET")), &target)
	res, err := dblifecycle.Call(ctx, os.Getenv("DBTS_SESSION"), dblifecycle.Request{
		Op: os.Getenv("DBTS_OP"), Target: target, Input: input, Epoch: epoch, Yes: os.Getenv("DBTS_YES") == "1",
		RequestID: os.Getenv("DBTS_REQUEST_ID"), Wait: 5 * time.Second,
	})
	if err != nil {
		res = dblifecycle.Response{Error: &dblifecycle.OwnerError{Code: dbstore.Code(err), Message: err.Error()}}
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

type tsClient struct {
	t       *testing.T
	session string
	target  dbstore.Target
}

type tsCall struct {
	agent, op, requestID string
	in                   Input
	epoch                uint64
	yes                  bool
}

func (c tsClient) cmd(r tsCall) *exec.Cmd {
	in, _ := json.Marshal(r.in)
	tb, _ := json.Marshal(c.target)
	cmd := exec.Command(os.Args[0], "-test.run=^TestTSClientHelper$")
	yes := "0"
	if r.yes {
		yes = "1"
	}
	cmd.Env = append(os.Environ(), "DBTS_CLIENT_HELPER=1", "DBTS_SESSION="+c.session, "DBTS_TARGET="+string(tb),
		"DBTS_OP="+r.op, "DBTS_INPUT="+string(in), "DBTS_EPOCH="+strconv.FormatUint(r.epoch, 10), "DBTS_YES="+yes,
		"DBTS_REQUEST_ID="+r.requestID, "BASHY_EPISODE="+r.agent)
	return cmd
}

func (c tsClient) do(r tsCall) dblifecycle.Response {
	c.t.Helper()
	b, err := c.cmd(r).Output()
	if err != nil {
		c.t.Fatalf("client %s %s: %v %s", r.agent, r.op, err, b)
	}
	var res dblifecycle.Response
	dec := json.NewDecoder(strings.NewReader(strings.SplitN(string(b), "\n", 2)[0]))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		c.t.Fatalf("client output %q: %v", b, err)
	}
	return res
}

func (c tsClient) ok(r tsCall) dblifecycle.Response {
	c.t.Helper()
	res := c.do(r)
	if !res.OK {
		c.t.Fatalf("%s %s: %+v", r.agent, r.op, res.Error)
	}
	return res
}

func (c tsClient) code(r tsCall) string {
	c.t.Helper()
	res := c.do(r)
	if res.Error == nil {
		return ""
	}
	return res.Error.Code
}

func startTSOwner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTSOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "DBTS_OWNER_HELPER=1", "DBTS_READY="+ready)
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

func tsEpochOf(t *testing.T, res dblifecycle.Response) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(string(res.Result.(map[string]any)["epoch"].(json.Number)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func tsPointsJSON(res dblifecycle.Response) string {
	r := res.Result.(map[string]any)
	pts := r["points"]
	b, _ := json.Marshal(pts)
	// Owner responses cross JSON, so tagged values decode to plain maps.
	return string(b)
}

func TestIndependentTSClientsShareOwnerMemoryDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process test; the manager batches process gates")
	}
	session := startTSOwner(t)
	c := tsClient{t: t, session: session, target: dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: session, Name: "shared"}}
	c.ok(tsCall{agent: "agent-a", op: "create"})
	// Unconfirmed destructive retain is refused before any data moves.
	if code := c.code(tsCall{agent: "agent-a", op: OpRetain, in: Input{Series: "w", Retention: "1h"}}); code != "confirmation_required" {
		t.Fatalf("unconfirmed retain = %s", code)
	}

	// Two writer processes ingest distinct stamps at once; a reader reads.
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for _, agent := range []string{"agent-a", "agent-b"} {
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(agent string, i int) {
				defer wg.Done()
				var pts []Point
				for j := 0; j < 2; j++ {
					ns := int64(1_700_000_000_000_000_000 + i*1_000_000_000 + j)
					if agent == "agent-b" {
						ns += 500_000_000
					}
					pts = append(pts, tsPoint(fmt.Sprintf(`{"t":"int","v":"%d"}`, ns), f64v(float64(i*2+j))))
				}
				res := c.do(tsCall{agent: agent, op: OpIngest, requestID: fmt.Sprintf("%s-%d", agent, i),
					in: Input{Series: "w", Points: pts}})
				if !res.OK {
					errs <- fmt.Sprintf("%s %d: %+v", agent, i, res.Error)
				}
			}(agent, i)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if res := c.do(tsCall{agent: "agent-c", op: OpRange, in: Input{Series: "w"}}); !res.OK {
			errs <- fmt.Sprintf("reader: %+v", res.Error)
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	got := tsPointsJSON(c.ok(tsCall{agent: "agent-c", op: OpRange, in: Input{Series: "w", Limit: 100}}))
	var pts []any
	_ = json.Unmarshal([]byte(got), &pts)
	if len(pts) != 20 {
		t.Fatalf("after concurrent writes: %d points", len(pts))
	}
	// A timed-out client may retry with its request id: nothing applies twice.
	replay := tsCall{agent: "agent-a", op: OpIngest, requestID: "agent-a-0", in: Input{Series: "w", Points: []Point{
		tsPoint(fmt.Sprintf(`{"t":"int","v":"%d"}`, 1_700_000_000_000_000_000), f64v(0)),
		tsPoint(fmt.Sprintf(`{"t":"int","v":"%d"}`, 1_700_000_000_000_000_001), f64v(1)),
	}}}
	c.ok(replay)
	got = tsPointsJSON(c.ok(tsCall{agent: "agent-c", op: OpRange, in: Input{Series: "w", Limit: 100}}))
	pts = nil
	_ = json.Unmarshal([]byte(got), &pts)
	if len(pts) != 20 {
		t.Fatalf("replay applied again: %d points", len(pts))
	}
	// The same request id with other input is a conflict, not a silent skip.
	reused := replay
	reused.in.Points = reused.in.Points[:1]
	if code := c.code(reused); code != "conflict" {
		t.Fatalf("request id reused with other input = %s", code)
	}
	// A contested stamp from a second process is a conflict, not a lost update.
	dup := tsCall{agent: "agent-b", op: OpIngest, in: Input{Series: "w", Points: []Point{
		tsPoint(fmt.Sprintf(`{"t":"int","v":"%d"}`, 1_700_000_000_000_000_000), f64v(99)),
	}}}
	if code := c.code(dup); code != "conflict" {
		t.Fatalf("contested stamp = %s", code)
	}

	// Exclusive hold: agent-a holds; agent-b's retain is refused with the
	// holder named; a stale epoch is refused; then the holder retains.
	hold := c.ok(tsCall{agent: "agent-a", op: "hold"})
	epoch := tsEpochOf(t, hold)
	if code := c.code(tsCall{agent: "agent-b", op: OpRetain, yes: true, in: Input{Series: "w", Retention: "1h"}}); code != "claim_held" {
		t.Fatalf("other agent retain during hold = %s", code)
	}
	if code := c.code(tsCall{agent: "agent-a", op: OpRange, epoch: epoch + 1, in: Input{Series: "w"}}); code != "stale_epoch" {
		t.Fatalf("holder with wrong epoch = %s", code)
	}
	c.ok(tsCall{agent: "agent-a", op: OpRange, epoch: epoch, in: Input{Series: "w"}})
	c.ok(tsCall{agent: "agent-a", op: "release", epoch: epoch})

	// Memory -> persistent through the owner and a direct import.
	out := filepath.Join(t.TempDir(), "mem-export")
	c.ok(tsCall{agent: "agent-b", op: OpExport, in: Input{Out: out}})
	dst := filepath.Join(t.TempDir(), "copy.db")
	pt := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: dst}
	if _, err := (dblifecycle.Service{}).Create(context.Background(), pt, time.Second); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(Input{In: out, Replace: true})
	if _, _, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpImport, in, true, time.Second); err != nil {
		t.Fatal(err)
	}
	q, _ := json.Marshal(Input{Series: "w"})
	_, res, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpRange, q, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Result.(map[string]any)["points"].([]any)); n != 20 {
		t.Fatalf("persistent copy: %d points", n)
	}

	// The owner ends; its memory database is explicitly gone.
	c.ok(tsCall{agent: "agent-a", op: "stop", yes: true})
	time.Sleep(100 * time.Millisecond)
	if code := c.code(tsCall{agent: "agent-b", op: OpRange, in: Input{Series: "w"}}); code != "lifetime_ended" {
		t.Fatalf("after stop = %s", code)
	}
}

func TestIndependentTSProcessesSharePersistentFileDirectly(t *testing.T) {
	if testing.Short() {
		t.Skip("independent-process test; the manager batches process gates")
	}
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	p := filepath.Join(t.TempDir(), "direct.db")
	pt := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
	if _, err := (dblifecycle.Service{}).Create(context.Background(), pt, time.Second); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([]string, 3)
	for i, spec := range []struct{ op, input string }{
		{OpIngest, `{"series":"d","points":[{"ts":{"t":"int","v":"1000"},"value":{"t":"f64","v":1}}]}`},
		{OpIngest, `{"series":"d","points":[{"ts":{"t":"int","v":"2000"},"value":{"t":"f64","v":2}}]}`},
		{OpRange, `{"series":"d"}`},
	} {
		wg.Add(1)
		go func(i int, op, input string) {
			defer wg.Done()
			var last string
			for j := 0; j < 25; j++ {
				cmd := exec.Command(os.Args[0], "-test.run=^TestTSClientHelper$")
				cmd.Env = append(os.Environ(), "DBTS_CLIENT_HELPER=1", "DBTS_DIRECT_PATH="+p, "DBTS_REPEAT=1", "DBTS_OP="+op, "DBTS_INPUT="+input)
				b, _ := cmd.CombinedOutput()
				last = string(b)
			}
			outs[i] = last
		}(i, spec.op, spec.input)
	}
	wg.Wait()
	for i, o := range outs {
		// The two ingest processes retry the same stamps: every repeat
		// after the first is a conflict, which is also an honest outcome.
		if !strings.HasPrefix(o, "ok") && !strings.HasPrefix(o, "conflict") {
			t.Errorf("process %d: %s", i, o)
		}
	}
	q, _ := json.Marshal(Input{Series: "d"})
	_, res, err := (dblifecycle.Service{}).RunCategory(context.Background(), pt, OpRange, q, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Result.(map[string]any)["points"].([]any)); n != 2 {
		t.Fatalf("no lost writes: %d points", n)
	}
}
