package dblifecycle

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func TestOwnerProcessHelper(t *testing.T) {
	if os.Getenv("BASHY_DB_OWNER_HELPER") != "1" {
		return
	}
	if err := Serve(context.Background(), os.Getenv("BASHY_DB_READY"), 0); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerClientHelper(t *testing.T) {
	if os.Getenv("BASHY_DB_CLIENT_HELPER") != "1" {
		return
	}
	id := os.Getenv("BASHY_DB_TEST_SESSION")
	op := os.Getenv("BASHY_DB_CLIENT_OP")
	if op == "" {
		op = "show"
	}
	epoch, _ := strconv.ParseUint(os.Getenv("BASHY_DB_CLIENT_EPOCH"), 10, 64)
	attachmentID := os.Getenv("BASHY_DB_CLIENT_ATTACHMENT")
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "shared"}
	if p := os.Getenv("BASHY_DB_CLIENT_PATH"); p != "" {
		target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: p}
	}
	res, err := Call(context.Background(), id, Request{Op: op, Epoch: epoch, Target: target, Attachment: attachmentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		t.Fatal(err)
	}
}

func ownerClientProcess(t *testing.T, id, episode, op, attachmentID string) Response {
	t.Helper()
	client := exec.Command(os.Args[0], "-test.run=^TestOwnerClientHelper$")
	client.Env = append(os.Environ(), "BASHY_DB_CLIENT_HELPER=1", "BASHY_DB_TEST_SESSION="+id, "BASHY_DB_CLIENT_OP="+op, "BASHY_DB_CLIENT_ATTACHMENT="+attachmentID, "BASHY_EPISODE="+episode)
	b, err := client.Output()
	if err != nil {
		t.Fatalf("client %s %s: %v", episode, op, err)
	}
	var res Response
	if err := json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestOwnerHoldFencesOtherProcess(t *testing.T) {
	id, cmd := startOwnerForTest(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "shared"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, e := Call(ctx, id, Request{Op: "create", Target: target}); e != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, e)
	}
	hold, err := Call(ctx, id, Request{Op: "hold", Target: target})
	if err != nil || !hold.OK {
		t.Fatalf("hold: %+v %v", hold, err)
	}
	epoch := epochOf(t, hold)
	client := exec.Command(os.Args[0], "-test.run=^TestOwnerClientHelper$")
	client.Env = append(os.Environ(), "BASHY_DB_CLIENT_HELPER=1", "BASHY_DB_TEST_SESSION="+id, "BASHY_EPISODE=other-agent-episode")
	b, err := client.Output()
	if err != nil {
		t.Fatalf("other client: %v", err)
	}
	var blocked Response
	if err := json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Error == nil || blocked.Error.Code != "claim_held" {
		t.Fatalf("other agent was not excluded: %+v", blocked)
	}
	stale, err := Call(ctx, id, Request{Op: "show", Target: target})
	if err != nil || stale.Error == nil || stale.Error.Code != "stale_epoch" {
		t.Fatalf("missing epoch: %+v %v", stale, err)
	}
	valid, err := Call(ctx, id, Request{Op: "show", Target: target, Epoch: epoch})
	if err != nil || !valid.OK {
		t.Fatalf("holder read: %+v %v", valid, err)
	}
	released, err := Call(ctx, id, Request{Op: "release", Target: target, Epoch: epoch})
	if err != nil || !released.OK {
		t.Fatalf("release: %+v %v", released, err)
	}
	if r, e := Call(ctx, id, Request{Op: "stop", Yes: true}); e != nil || !r.OK {
		t.Fatalf("stop: %+v %v", r, e)
	}
	_ = cmd.Wait()
}

func TestOwnerMutationAdmissionCancellation(t *testing.T) {
	o := &owner{mutationGate: dbstore.NewGate()}
	leave, err := o.mutationGate.Enter(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	res := o.executeReplay(ctx, Request{Op: "create", Wait: time.Second})
	if res.Error == nil || res.Error.Code != "timeout" {
		t.Fatalf("blocked mutation = %+v", res)
	}
}

func TestExpiredAttachmentRejected(t *testing.T) {
	holder := principal.Ref{Name: "agent", Episode: "ep-expired"}
	o := &owner{attachments: map[string]attachment{"a": {holder: holder, expires: time.Now().Add(-time.Second)}}}
	r := o.execute(context.Background(), Request{Op: "status", Holder: holder, Attachment: "a"})
	if r.Error == nil || r.Error.Code != "invalid_input" {
		t.Fatalf("expired attachment: %+v", r)
	}
}

func TestStaleOwnerHoldClosesBackendWithoutReleasingSuccessor(t *testing.T) {
	id, cmd := startOwnerForTest(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "shared"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, e := Call(ctx, id, Request{Op: "create", Target: target}); e != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, e)
	}
	r, err := Call(ctx, id, Request{Op: "hold", Target: target})
	if err != nil || !r.OK {
		t.Fatalf("hold: %+v %v", r, err)
	}
	epoch := epochOf(t, r)
	identity, err := dbstore.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	ref := coord.Ref{Kind: "db", Name: identity.ID}
	if err := coord.ReleaseRef(ctx, ref, coord.Self(), epoch); err != nil {
		t.Fatal(err)
	}
	other := principal.Ref{Name: "other", Episode: "other-stale-test", Host: coord.Self().Host}
	grant, err := coord.AcquireRef(ctx, coord.Request{Ref: ref, Holder: other, Mode: coord.ModeLease})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := Call(ctx, id, Request{Op: "release", Target: target, Epoch: epoch})
	if err != nil || stale.Error == nil || stale.Error.Code != "stale_epoch" {
		t.Fatalf("stale release: %+v %v", stale, err)
	}
	if _, err := coord.Refresh(ctx, ref, other, grant.Epoch); err != nil {
		t.Fatalf("successor claim lost: %v", err)
	}
	if err := coord.ReleaseRef(ctx, ref, other, grant.Epoch); err != nil {
		t.Fatal(err)
	}
	if r, e := Call(ctx, id, Request{Op: "show", Target: target}); e != nil || !r.OK {
		t.Fatalf("native handle was not freed: %+v %v", r, e)
	}
	if r, e := Call(ctx, id, Request{Op: "stop", Yes: true}); e != nil || !r.OK {
		t.Fatalf("stop: %+v %v", r, e)
	}
	_ = cmd.Wait()
}

func TestPersistentOwnerRestartAndMemoryOwnerLoss(t *testing.T) {
	root := t.TempDir()
	id, cmd := startOwnerForTest(t)
	path := filepath.Join(root, "store.db")
	persistent := dbstore.Target{Engine: "bbolt", Mode: dbstore.Persistent, Path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, e := Call(ctx, id, Request{Op: "create", Target: persistent}); e != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, e)
	}
	memory := dbstore.Target{Engine: "bbolt", Mode: dbstore.Memory, Session: id, Name: "ephemeral"}
	if r, e := Call(ctx, id, Request{Op: "create", Target: memory}); e != nil || !r.OK {
		t.Fatalf("create memory: %+v %v", r, e)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := Call(ctx, id, Request{Op: "show", Target: memory}); dbstore.Code(err) != "lifetime_ended" {
		t.Fatalf("old memory owner = %v", err)
	}
	newID, newCmd := startOwnerForTest(t)
	if newID == id {
		t.Fatal("owner session ID was reused")
	}
	if r, e := Call(ctx, newID, Request{Op: "show", Target: persistent}); e != nil || !r.OK {
		t.Fatalf("persistent reopen after owner crash: %+v %v", r, e)
	}
	if r, e := Call(ctx, newID, Request{Op: "stop"}); e != nil || !r.OK {
		t.Fatalf("new owner stop: %+v %v", r, e)
	}
	_ = newCmd.Wait()
}

func TestPersistentAliasCannotBypassOwnerHold(t *testing.T) {
	id, cmd := startOwnerForTest(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "data.db")
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, e := Call(ctx, id, Request{Op: "create", Target: target}); e != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, e)
	}
	hold, err := Call(ctx, id, Request{Op: "hold", Target: target})
	if err != nil || !hold.OK {
		t.Fatalf("hold: %+v %v", hold, err)
	}
	epoch := epochOf(t, hold)
	alias := filepath.Join(root, "sub", "..", "data.db")
	client := exec.Command(os.Args[0], "-test.run=^TestOwnerClientHelper$")
	client.Env = append(os.Environ(), "BASHY_DB_CLIENT_HELPER=1", "BASHY_DB_TEST_SESSION="+id, "BASHY_DB_CLIENT_PATH="+alias, "BASHY_EPISODE=alias-other-agent")
	b, err := client.Output()
	if err != nil {
		t.Fatal(err)
	}
	var blocked Response
	if err := json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Error == nil || blocked.Error.Code != "claim_held" {
		t.Fatalf("alias bypass: %+v", blocked)
	}
	if r, e := Call(ctx, id, Request{Op: "release", Target: target, Epoch: epoch}); e != nil || !r.OK {
		t.Fatalf("release: %+v %v", r, e)
	}
	if r, e := Call(ctx, id, Request{Op: "stop"}); e != nil || !r.OK {
		t.Fatalf("stop: %+v %v", r, e)
	}
	_ = cmd.Wait()
}

func startOwnerForTest(t *testing.T) (string, *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "BASHY_DB_OWNER_HELPER=1", "BASHY_DB_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			return string(b), cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owner did not become ready")
	return "", nil
}

func TestOwnerMemoryLifetimeAndReplay(t *testing.T) {
	id, cmd := startOwnerForTest(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "shared"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := Request{Op: "create", Target: target, RequestID: "create-once"}
	first, err := Call(ctx, id, req)
	if err != nil || !first.OK {
		t.Fatalf("create: %+v %v", first, err)
	}
	replay, err := Call(ctx, id, req)
	if err != nil || !replay.OK || replay.RequestID != "create-once" {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	show, err := Call(ctx, id, Request{Op: "show", Target: target})
	if err != nil || !show.OK {
		t.Fatalf("show: %+v %v", show, err)
	}
	// Two independent client processes reach the same owner-held memory target.
	for i := 0; i < 2; i++ {
		client := exec.Command(os.Args[0], "-test.run=^TestOwnerClientHelper$")
		client.Env = append(os.Environ(), "BASHY_DB_CLIENT_HELPER=1", "BASHY_DB_TEST_SESSION="+id)
		b, err := client.Output()
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if !strings.Contains(string(b), `"ok":true`) {
			t.Fatalf("client %d: %s", i, b)
		}
	}
	changed := req
	changed.Target.Name = "other"
	conflict, err := Call(ctx, id, changed)
	if err != nil || conflict.Error == nil || conflict.Error.Code != "conflict" {
		t.Fatalf("request-id reuse: %+v %v", conflict, err)
	}
	stop, err := Call(ctx, id, Request{Op: "stop", Yes: true})
	if err != nil || !stop.OK {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	_ = cmd.Wait()
	_, err = Call(ctx, id, Request{Op: "show", Target: target})
	if dbstore.Code(err) != "lifetime_ended" {
		t.Fatalf("ended session = %v", err)
	}
}

func TestOwnerAttachmentsAndBusyStop(t *testing.T) {
	id, cmd := startOwnerForTest(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "shared"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, e := Call(ctx, id, Request{Op: "create", Target: target}); e != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, e)
	}
	a := ownerClientProcess(t, id, "attached-client-a", "attach", "")
	b := ownerClientProcess(t, id, "attached-client-b", "attach", "")
	if !a.OK || !b.OK {
		t.Fatalf("attach a/b: %+v %+v", a, b)
	}
	aID := a.Result.(map[string]any)["attachment_id"].(string)
	bID := b.Result.(map[string]any)["attachment_id"].(string)
	busy, err := Call(ctx, id, Request{Op: "stop"})
	if err != nil || busy.Error == nil || busy.Error.Code != "busy" {
		t.Fatalf("stop with attachments: %+v %v", busy, err)
	}
	if r := ownerClientProcess(t, id, "attached-client-a", "detach", aID); !r.OK {
		t.Fatalf("detach a: %+v", r)
	}
	if r := ownerClientProcess(t, id, "attached-client-b", "show", bID); !r.OK {
		t.Fatalf("b lost database after a disconnect: %+v", r)
	}
	if r := ownerClientProcess(t, id, "attached-client-b", "detach", bID); !r.OK {
		t.Fatalf("detach b: %+v", r)
	}
	if r, e := Call(ctx, id, Request{Op: "stop"}); e != nil || !r.OK {
		t.Fatalf("stop after detach: %+v %v", r, e)
	} else if r.Op != "session.stop" || r.MutationClass != "destructive" || !slices.Contains(r.Effects, "proc.stop") {
		t.Fatalf("stop envelope: %+v", r)
	}
	_ = cmd.Wait()
}

func TestOwnerSessionOperationMetadata(t *testing.T) {
	for _, tt := range []struct{ op, name, class string }{
		{"attach", "session.attach", "write"},
		{"detach", "session.detach", "write"},
		{"status", "session.show", "read"},
		{"stop", "session.stop", "destructive"},
	} {
		s := ownerSpec(Request{Op: tt.op})
		if s.Name != tt.name || s.MutationClass != tt.class {
			t.Errorf("%s = %+v", tt.op, s)
		}
	}
}

// epochOf reads a hold epoch from an owner response; Call keeps numbers as
// json.Number.
func epochOf(t *testing.T, res Response) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(string(res.Result.(map[string]any)["epoch"].(json.Number)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Call decodes owner responses with UseNumber: a category result keeps big
// integers and decimal text exactly, and the hold epoch consumer reads the
// same json.Number through acquire, nested refresh and release. The owner
// runs in this process, so no subprocess is involved.
func TestOwnerCallPreservesNumberLexemes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	RegisterCategoryOp("test.echo", CategoryOp{
		Access: func(json.RawMessage) (Access, error) { return AccessRead, nil },
		Spec: func(dbstore.Mode, Access) Operation {
			return Operation{Name: "test.echo", MutationClass: "read", Effects: []string{}}
		},
		Run: func(_ context.Context, _ dbstore.Handle, input json.RawMessage, _ Access) (Outcome, error) {
			return Outcome{Result: input}, nil
		},
	})
	t.Cleanup(func() { categoryOps.Delete("test.echo") })
	ready := filepath.Join(dir, "ready")
	serveCtx, stopServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, ready, 0) }()
	t.Cleanup(func() { stopServe(); <-served })
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			id = string(b)
		}
	}
	if id == "" {
		t.Fatal("owner did not become ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "numbers"}
	if r, err := Call(ctx, id, Request{Op: "create", Target: target}); err != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, err)
	}
	echo, err := Call(ctx, id, Request{Op: "test.echo", Target: target, Input: json.RawMessage(`{"big":12345678901234567890,"dec":1.50,"exp":1E+2,"nested":[-0.0]}`)})
	if err != nil || !echo.OK {
		t.Fatalf("echo: %+v %v", echo, err)
	}
	got := echo.Result.(map[string]any)
	for key, want := range map[string]string{"big": "12345678901234567890", "dec": "1.50", "exp": "1E+2"} {
		if n, ok := got[key].(json.Number); !ok || n.String() != want {
			t.Fatalf("%s = %#v, want json.Number %s", key, got[key], want)
		}
	}
	if n, ok := got["nested"].([]any)[0].(json.Number); !ok || n.String() != "-0.0" {
		t.Fatalf("nested = %#v", got["nested"])
	}
	hold, err := Call(ctx, id, Request{Op: "hold", Target: target})
	if err != nil || !hold.OK {
		t.Fatalf("hold: %+v %v", hold, err)
	}
	epoch := epochOf(t, hold)
	refresh, err := Call(ctx, id, Request{Op: "hold", Target: target, Epoch: epoch})
	if err != nil || !refresh.OK || refresh.Result.(map[string]any)["nested"] != true || epochOf(t, refresh) != epoch {
		t.Fatalf("refresh: %+v %v", refresh, err)
	}
	if stale, err := Call(ctx, id, Request{Op: "release", Target: target, Epoch: epoch + 1}); err != nil || stale.OK || stale.Error.Code != "stale_epoch" {
		t.Fatalf("stale release: %+v %v", stale, err)
	}
	release, err := Call(ctx, id, Request{Op: "release", Target: target, Epoch: epoch})
	if err != nil || !release.OK || release.Result.(map[string]any)["released"] != true {
		t.Fatalf("release: %+v %v", release, err)
	}
}
