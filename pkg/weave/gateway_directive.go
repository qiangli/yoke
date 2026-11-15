package weave

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/chat"
)

// Gateway directives use the task wire contract in docs/vendor-chat-gateway-mvp.md.
// The record is shared by the pull conductor and the loopback sync handler.
type gatewayDirective struct {
	Verb           string `json:"verb"`
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Host           string `json:"host"`
	Caller         string `json:"caller"`
	Args           struct {
		Argv       []string `json:"argv"`
		Worker     string   `json:"worker"`
		Prompt     string   `json:"prompt"`
		ApprovalID string   `json:"approval_id"`
		Digest     string   `json:"digest"`
		Decision   string   `json:"decision"`
		Reason     string   `json:"reason"`
		Text       string   `json:"text"`
	} `json:"args"`
}

type gatewayTaskRecord struct {
	TaskID            string         `json:"task_id"`
	State             string         `json:"state"`
	Reason            string         `json:"reason,omitempty"`
	Result            *gatewayResult `json:"result,omitempty"`
	ResultSent        bool           `json:"result_sent,omitempty"`
	TerminalStateSent bool           `json:"terminal_state_sent,omitempty"`
	// ApprovalID and Digest bind a needs-approval task to its ask approval,
	// so a crash between raising and settling can re-emit the same event.
	ApprovalID   string `json:"approval_id,omitempty"`
	Digest       string `json:"digest,omitempty"`
	ApprovalSent bool   `json:"approval_sent,omitempty"`
	// Route is "sync" when the loopback sync handler claimed the key. Such a
	// record is executed by the sync handler only; the pull conductor adopts
	// it and publishes its outcome instead of running it again.
	Route string `json:"route,omitempty"`
	// SyncReply is the sync handler's verbatim reply, so a duplicate sync
	// call answers with the same shape the first call returned.
	SyncReply json.RawMessage `json:"sync_reply,omitempty"`
}

// gatewayApprovalWait bounds how long a gated run waits for its approval
// before the conductor denies it as unanswered. Tests shrink it.
var gatewayApprovalWait = 10 * time.Minute

type gatewayResult struct {
	Text     string `json:"text"`
	Exit     int    `json:"exit"`
	Verified bool   `json:"verified"`
}

type gatewayRun struct{ cancel context.CancelFunc }

type gatewayConductor struct {
	client  SessionClient
	dir     string
	workdir string
	mu      sync.Mutex
	wg      sync.WaitGroup
	active  map[string]gatewayRun
	// execute is replaceable in tests; a successful delegated answer is asserted,
	// while a deterministic command's exit code is verified by the host.
	execute func(context.Context, gatewayDirective) (string, int, bool)
}

func newGatewayConductor(client SessionClient, dir string) *gatewayConductor {
	g := &gatewayConductor{client: client, dir: dir, active: make(map[string]gatewayRun)}
	g.execute = func(ctx context.Context, d gatewayDirective) (string, int, bool) {
		return executeGatewayDirective(ctx, d, g.workdir)
	}
	return g
}

func gatewayRecordPath(dir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "gateway-tasks", hex.EncodeToString(sum[:])+".json")
}

func gatewayStateDir() (string, error) {
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".bashy")
	}
	return filepath.Join(home, "gateway"), nil
}

func gatewayCursorPath(dir, taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return filepath.Join(dir, "gateway-cursors", hex.EncodeToString(sum[:]))
}

func readGatewayRecord(path string) (gatewayTaskRecord, error) {
	var r gatewayTaskRecord
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}

func writeGatewayRecord(path string, r gatewayTaskRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// claimGatewayTask is the common sync/pull deduplication point. A sync caller
// must claim before executing; a later pull sees the same task id and skips.
func claimGatewayTask(dir, key, taskID string) (gatewayTaskRecord, bool, error) {
	return claimGatewayTaskRoute(dir, key, taskID, gatewayTaskRecord{TaskID: taskID, State: "queued"})
}

func claimGatewayTaskRoute(dir, key, taskID string, r gatewayTaskRecord) (gatewayTaskRecord, bool, error) {
	if key == "" || taskID == "" {
		return gatewayTaskRecord{}, false, errors.New("task_id and idempotency_key are required")
	}
	path := gatewayRecordPath(dir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return gatewayTaskRecord{}, false, err
	}
	b, _ := json.Marshal(r)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		// The winner may still be writing the just-created record. Wait for a
		// complete value; an incomplete record fails closed after the deadline.
		for i := 0; i < 100; i++ {
			r, err = readGatewayRecord(path)
			if err == nil && r.TaskID != "" {
				return r, false, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return gatewayTaskRecord{}, false, fmt.Errorf("idempotency record is incomplete: %v", err)
	}
	if err != nil {
		return gatewayTaskRecord{}, false, err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return r, err == nil, err
}

// ClaimGatewayTask reserves one task id for an idempotency key before either
// route starts work. The returned id is authoritative for duplicate calls.
// dir is a host-scoped gateway state directory.
func ClaimGatewayTask(dir, key, taskID string) (existingTaskID string, fresh bool, err error) {
	r, fresh, err := claimGatewayTask(dir, key, taskID)
	return r.TaskID, fresh, err
}

// ClaimGatewayTaskOnHost is the sync handler's entry point. It shares the
// conductor's host-scoped record so the pull fallback cannot launch twice.
// A fresh claim is recorded as running on the sync route: the caller must
// execute and then call CompleteGatewayTaskOnHost. A pull directive for the
// same key adopts the record and publishes its result without executing.
func ClaimGatewayTaskOnHost(key, taskID string) (existingTaskID string, fresh bool, err error) {
	dir, err := gatewayStateDir()
	if err != nil {
		return "", false, err
	}
	r, fresh, err := claimGatewayTaskRoute(dir, key, taskID, gatewayTaskRecord{TaskID: taskID, State: "running", Route: "sync"})
	return r.TaskID, fresh, err
}

// GatewayTask is the host record as the sync handler sees it.
type GatewayTask struct {
	TaskID    string
	State     string
	HasResult bool
	Text      string
	Exit      int
	Verified  bool
	// SyncReply is the sync handler's stored reply, if it recorded one.
	SyncReply json.RawMessage
}

// GatewayTaskOnHost reads the host record for an idempotency key, so a
// duplicate sync call can answer from it instead of executing.
func GatewayTaskOnHost(key string) (GatewayTask, error) {
	dir, err := gatewayStateDir()
	if err != nil {
		return GatewayTask{}, err
	}
	r, err := readGatewayRecord(gatewayRecordPath(dir, key))
	if err != nil {
		return GatewayTask{}, err
	}
	t := GatewayTask{TaskID: r.TaskID, State: r.State, SyncReply: r.SyncReply}
	if r.Result != nil {
		t.HasResult = true
		t.Text, t.Exit, t.Verified = r.Result.Text, r.Result.Exit, r.Result.Verified
	}
	return t, nil
}

// CompleteGatewayTaskOnHost writes a sync-route outcome into the same host
// record the pull conductor publishes from (state, text, exit, verified).
// reply is the sync handler's verbatim answer for duplicate sync calls. A
// cancellation that already won keeps its terminal state.
func CompleteGatewayTaskOnHost(key, taskID, text string, exit int, verified bool, reply []byte) error {
	dir, err := gatewayStateDir()
	if err != nil {
		return err
	}
	return completeSyncGatewayTask(dir, key, taskID, text, exit, verified, reply)
}

func completeSyncGatewayTask(dir, key, taskID, text string, exit int, verified bool, reply []byte) error {
	path := gatewayRecordPath(dir, key)
	r, err := readGatewayRecord(path)
	if err != nil {
		return err
	}
	if r.TaskID != taskID {
		return fmt.Errorf("idempotency key belongs to task %s", r.TaskID)
	}
	if r.Route != "sync" {
		return errors.New("idempotency key was not claimed by the sync route")
	}
	if r.State == "cancelled" || r.Result != nil {
		return nil
	}
	r.State = "done"
	if exit != 0 {
		r.State = "failed"
	}
	r.Result = &gatewayResult{Text: text, Exit: exit, Verified: verified}
	if len(reply) > 0 {
		r.SyncReply = append(json.RawMessage(nil), reply...)
	}
	return writeGatewayRecord(path, r)
}

// gatewayAdoptedPath marks a sync-claimed record as owed to the pull route.
// It is a separate file so the conductor never rewrites a record the sync
// handler (another process) may still be completing.
func gatewayAdoptedPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".pull"
}

func gatewayAdopted(recordPath string) bool {
	_, err := os.Stat(gatewayAdoptedPath(recordPath))
	return err == nil
}

// adoptSync answers a pull directive for a key the sync route claimed: it
// never executes. A stored result is published now; a running claim reports
// its state and flushOutbox publishes the result once the sync handler
// records it.
func (g *gatewayConductor) adoptSync(ctx context.Context, key string) error {
	path := gatewayRecordPath(g.dir, key)
	if err := os.WriteFile(gatewayAdoptedPath(path), nil, 0o600); err != nil {
		return err
	}
	g.mu.Lock()
	r, err := readGatewayRecord(path)
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if r.Result != nil || r.State == "cancelled" {
		return g.flushRecordPath(ctx, path)
	}
	return g.emit(ctx, r.TaskID, "state", map[string]any{"state": r.State, "reason": r.Reason})
}

func (g *gatewayConductor) emit(ctx context.Context, id, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = g.client.AppendEvent(ctx, id, AppendEventReq{Kind: kind, Summary: kind, Detail: b})
	return err
}

func gatewayDirectiveSeq(ev Event) any {
	if ev.Seq != 0 {
		return ev.Seq
	}
	return ev.ID
}

func (g *gatewayConductor) setState(ctx context.Context, key, id, state, reason string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := gatewayRecordPath(g.dir, key)
	r, err := readGatewayRecord(path)
	if err != nil {
		return err
	}
	if r.TaskID != id {
		return nil
	}
	if r.State == "cancelled" || r.State == "done" || r.State == "failed" {
		return nil
	}
	r.State = state
	if err := writeGatewayRecord(path, r); err != nil {
		return err
	}
	_ = g.emit(ctx, id, "state", map[string]any{"state": state, "reason": reason})
	return nil
}

func (g *gatewayConductor) apply(ctx context.Context, taskID string, ev Event) error {
	var d gatewayDirective
	if err := json.Unmarshal(ev.Detail, &d); err != nil {
		return err
	}
	if d.TaskID != taskID || d.Host != sessionHostName() {
		return nil
	}
	if d.Verb == "cancel" {
		if err := g.cancel(ctx, d); err != nil {
			return err
		}
		return g.emit(ctx, taskID, "ack", map[string]any{"directive_seq": gatewayDirectiveSeq(ev)})
	}
	if d.Verb == "approve" {
		if err := g.approve(ctx, ev, d); err != nil {
			return err
		}
		return g.emit(ctx, taskID, "ack", map[string]any{"directive_seq": gatewayDirectiveSeq(ev)})
	}
	if d.Verb == "say" {
		return g.emit(ctx, taskID, "ack", map[string]any{"directive_seq": gatewayDirectiveSeq(ev)})
	}
	if d.Verb != "run" && d.Verb != "delegate" {
		return fmt.Errorf("unknown gateway verb %q", d.Verb)
	}
	r, fresh, err := claimGatewayTask(g.dir, d.IdempotencyKey, d.TaskID)
	if err != nil {
		return err
	}
	_ = g.emit(ctx, taskID, "ack", map[string]any{"directive_seq": gatewayDirectiveSeq(ev), "task_id": r.TaskID})
	if !fresh {
		if r.Route == "sync" {
			return g.adoptSync(ctx, d.IdempotencyKey)
		}
		return nil
	}
	_ = g.emit(ctx, taskID, "state", map[string]any{"state": "queued", "reason": ""})
	runCtx, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.active[d.IdempotencyKey] = gatewayRun{cancel: cancel}
	g.wg.Add(1)
	g.mu.Unlock()
	go g.run(runCtx, d)
	return nil
}

func (g *gatewayConductor) run(ctx context.Context, d gatewayDirective) {
	defer g.wg.Done()
	effects, gated, gateErr := gatewayRunEffects(d)
	if gateErr == nil && g.setState(context.Background(), d.IdempotencyKey, d.TaskID, "running", "") != nil {
		return
	}
	g.mu.Lock()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey))
	g.mu.Unlock()
	if err != nil || r.State == "cancelled" {
		return
	}
	if gateErr != nil {
		g.finishResult(context.Background(), d, gateErr.Error(), 126, true, "gated-effect")
		return
	}
	// A destroy/spend/cred/priv effect never executes on a stored candidate:
	// it raises a digest-bound approval first and runs only after the owning
	// host settles it through ask.ApplyRemote.
	if len(gated) > 0 {
		if !g.awaitApproval(ctx, d, gated) {
			return
		}
	}
	if d.Verb == "run" && gatewayHasEffect(effects, atlas.EffWrite) {
		if err := g.emit(context.Background(), d.TaskID, "audit", map[string]any{"verb": d.Args.Argv[0], "effects": effects}); err != nil {
			g.finishResult(context.Background(), d, "write audit unavailable", 126, true, "audit-unavailable")
			return
		}
	}
	text, exit, verified := g.execute(ctx, d)
	g.finishResult(context.Background(), d, text, exit, verified, "")
}

// finishResult records a terminal run outcome unless a cancellation won the
// race, then hands the result and state to the outbox.
func (g *gatewayConductor) finishResult(ctx context.Context, d gatewayDirective, text string, exit int, verified bool, reason string) {
	g.mu.Lock()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey))
	if err != nil || r.State == "cancelled" {
		g.mu.Unlock()
		return
	}
	delete(g.active, d.IdempotencyKey)
	state := "done"
	if exit != 0 {
		state = "failed"
	}
	r.State = state
	r.Reason = reason
	r.Result = &gatewayResult{Text: text, Exit: exit, Verified: verified}
	if err := writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r); err != nil {
		fmt.Fprintln(os.Stderr, "weave conduct: persist result:", err)
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	_ = g.flushRecord(ctx, d.IdempotencyKey)
}

// cancelRecord moves a task to cancelled with a reason unless it already
// reached a terminal state, then hands the state to the outbox.
func (g *gatewayConductor) cancelRecord(ctx context.Context, d gatewayDirective, reason string) {
	g.mu.Lock()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey))
	if err != nil || r.State == "done" || r.State == "failed" || r.State == "cancelled" {
		g.mu.Unlock()
		return
	}
	delete(g.active, d.IdempotencyKey)
	r.State = "cancelled"
	r.Reason = reason
	if err := writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r); err != nil {
		fmt.Fprintln(os.Stderr, "weave conduct: persist cancellation:", err)
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	_ = g.flushRecord(ctx, d.IdempotencyKey)
}

// awaitApproval raises the digest-bound approval for a gated run, publishes
// the needs-approval state with its approval event, and waits for the owning
// host to settle it. It reports whether the run may proceed; a deny or an
// unanswered wait records the terminal state itself.
func (g *gatewayConductor) awaitApproval(ctx context.Context, d gatewayDirective, gated []string) bool {
	digest := ask.GatewayDigest(d.Args.Argv, d.Host, d.TaskID)
	req, err := ask.Raise(ask.ApprovalSpec{
		Source:  ask.SourceGateway,
		Argv:    append([]string(nil), d.Args.Argv...),
		Effects: append([]string(nil), gated...),
		Session: d.TaskID,
		Token:   digest,
		Prompt:  "gateway run " + strings.Join(d.Args.Argv, " ") + " on " + d.Host,
	})
	if err != nil {
		g.finishResult(context.Background(), d, "approval unavailable: "+err.Error(), 126, true, "approval-unavailable")
		return false
	}
	g.mu.Lock()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey))
	if err != nil || r.State == "cancelled" {
		g.mu.Unlock()
		return false
	}
	r.ApprovalID = req.ID
	r.Digest = digest
	if werr := writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r); werr != nil {
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()
	if g.setState(context.Background(), d.IdempotencyKey, d.TaskID, "needs-approval", "") != nil {
		return false
	}
	_ = g.emit(context.Background(), d.TaskID, "approval", map[string]any{
		"approval_id": req.ID,
		"digest":      digest,
		"summary":     req.ApprovalSummary(),
		"effects":     gated,
	})
	g.mu.Lock()
	if r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey)); err == nil {
		r.ApprovalSent = true
		_ = writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r)
	}
	g.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, gatewayApprovalWait)
	defer cancel()
	dec, err := ask.WaitDecision(waitCtx, req.ID)
	if err != nil {
		// A cancellation that arrived mid-wait owns the outcome; only an
		// actually unanswered approval is denied here.
		g.mu.Lock()
		cancelled := false
		if r, rerr := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey)); rerr == nil {
			cancelled = r.State == "cancelled"
		}
		g.mu.Unlock()
		if cancelled {
			return false
		}
		dec = ask.DenyUnanswered(req.ID, "gateway approval went unanswered")
	}
	if !dec.Allowed() {
		reason := "denied"
		if dec.Via == ask.ViaTimeout {
			reason = "approval-unanswered"
		}
		g.cancelRecord(context.Background(), d, reason)
		return false
	}
	return true
}

// gatewayGatedEffects returns the subset of a candidate's effects that needs
// a digest-bound approval, in atlas vocabulary order.
func gatewayGatedEffects(effects []string) []string {
	var gated []string
	for _, effect := range atlas.Effects() {
		switch effect {
		case atlas.EffDestroy, atlas.EffSpend, atlas.EffCred, atlas.EffPriv:
		default:
			continue
		}
		if gatewayHasEffect(effects, effect) {
			gated = append(gated, effect)
		}
	}
	return gated
}

func gatewayHasEffect(effects []string, want string) bool {
	for _, effect := range effects {
		if effect == want {
			return true
		}
	}
	return false
}

// The atlas is the exported source used by the MCP policy. A missing record
// has no known effect set and must never reach the process launcher.
// Gated effects (destroy, spend, cred, priv) are reported separately: the
// run raises a digest-bound approval for them instead of executing.
func gatewayRunEffects(d gatewayDirective) (effects, gated []string, err error) {
	if d.Verb != "run" {
		return nil, nil, nil
	}
	if len(d.Args.Argv) == 0 {
		return nil, nil, errors.New("argv required")
	}
	entry, ok := atlas.Lookup(d.Args.Argv[0])
	if !ok || len(entry.Effects) == 0 {
		return nil, nil, fmt.Errorf("unknown atlas verb %q", d.Args.Argv[0])
	}
	return entry.Effects, gatewayGatedEffects(entry.Effects), nil
}

func (g *gatewayConductor) cancel(ctx context.Context, d gatewayDirective) error {
	if d.IdempotencyKey == "" {
		return errors.New("cancel needs idempotency_key")
	}
	// A cancellation may arrive while the sync route is failing over, before
	// the pull run directive. Reserve its key now so that later run is skipped.
	if _, err := os.Stat(gatewayRecordPath(g.dir, d.IdempotencyKey)); errors.Is(err, os.ErrNotExist) {
		if _, _, err := claimGatewayTask(g.dir, d.IdempotencyKey, d.TaskID); err != nil {
			return err
		}
	}
	g.mu.Lock()
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey))
	if err != nil {
		g.mu.Unlock()
		return err
	}
	if r.TaskID != d.TaskID {
		g.mu.Unlock()
		return fmt.Errorf("idempotency key belongs to task %s", r.TaskID)
	}
	if r.Route == "sync" {
		// The cancel names this task on the pull timeline, so a durable
		// cancellation of a sync claim is retried by flushOutbox too.
		_ = os.WriteFile(gatewayAdoptedPath(gatewayRecordPath(g.dir, d.IdempotencyKey)), nil, 0o600)
	}
	if r.State == "done" || r.State == "failed" || r.State == "cancelled" {
		g.mu.Unlock()
		return nil
	}
	if run, ok := g.active[d.IdempotencyKey]; ok {
		run.cancel()
		delete(g.active, d.IdempotencyKey)
	}
	r.State = "cancelled"
	r.Reason = d.Args.Reason
	err = writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r)
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if err := g.emit(ctx, d.TaskID, "state", map[string]any{"state": "cancelled", "reason": d.Args.Reason}); err != nil {
		return nil // durable cancellation is retried by flushOutbox
	}
	g.mu.Lock()
	r.TerminalStateSent = true
	err = writeGatewayRecord(gatewayRecordPath(g.dir, d.IdempotencyKey), r)
	g.mu.Unlock()
	return err
}

func (g *gatewayConductor) approve(ctx context.Context, ev Event, d gatewayDirective) error {
	// A directive for another host's queue never settles here, even when it
	// reaches this method directly: the digest binds the owning host, and a
	// foreign decision must fail closed.
	if d.Host != sessionHostName() {
		return g.emit(ctx, d.TaskID, "result", map[string]any{"text": "approval is not for this host", "exit": 1, "verified": true})
	}
	// Trust only the server-stamped author, never the opaque caller in detail.
	tasks, err := g.client.ListTasks(ctx)
	if err != nil {
		return err
	}
	owner := ""
	for _, task := range tasks {
		if task.ID == d.TaskID {
			owner = task.Owner
			break
		}
	}
	if owner == "" || ev.Author == "" || (ev.Author != owner && !allowedRemoteAuthor(ev.Author, owner)) {
		return g.emit(ctx, d.TaskID, "result", map[string]any{"text": "approval author is not allowed", "exit": 1, "verified": true})
	}
	verdict := ask.VerdictDeny
	if d.Args.Decision == "approve" {
		verdict = ask.VerdictAllow
	} else if d.Args.Decision != "deny" {
		return errors.New("decision must be approve or deny")
	}
	by := "person:" + ev.Author
	if !strings.Contains(ev.Author, "@") {
		by += "@gateway"
	}
	_, err = ask.ApplyRemote(ask.RemoteDirective{
		ID: ev.ID, Approval: d.Args.ApprovalID, Host: d.Host,
		Token: d.Args.Digest, Verdict: verdict, By: by,
		At: time.Now().UTC().Format(time.RFC3339Nano),
	}, ask.RemotePolicy{Approvers: []string{by}})
	if err != nil {
		return g.emit(ctx, d.TaskID, "result", map[string]any{"text": err.Error(), "exit": 1, "verified": true})
	}
	return nil
}

func executeGatewayDirective(ctx context.Context, d gatewayDirective, workdir string) (string, int, bool) {
	argv := d.Args.Argv
	verified := true
	var launchEnv []string
	var delegateTool string
	if d.Verb == "delegate" {
		verified = false
		if strings.TrimSpace(d.Args.Prompt) == "" {
			return "prompt required", 2, true
		}
		worker := strings.TrimSpace(d.Args.Worker)
		if worker == "" {
			worker = strings.TrimSpace(os.Getenv("BASHY_GATEWAY_DEFAULT_WORKER"))
		}
		if worker != "claude" && worker != "codex" && worker != "agy" && worker != "muse" {
			return "worker missing or unsupported", 2, true
		}
		launch, err := agentlaunch.ResolveWithCatalog(worker, agentlaunch.Options{Workspace: workdir}, fleetCatalog)
		if err != nil {
			return err.Error(), 1, true
		}
		argv = launch.Argv(d.Args.Prompt)
		argv = agentlaunch.InsertBeforePrompt(argv, chat.DelegateSubscriptionArgs(launch.ToolName))
		delegateTool = launch.ToolName
		launchEnv = launch.Env
		if len(argv) == 0 {
			return "worker unavailable", 1, true
		}
	} else if len(argv) == 0 {
		return "argv required", 2, true
	} else {
		argv = append([]string{"bashy"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	if d.Verb == "delegate" {
		cmd.Env = chat.DelegateWorkerEnv(delegateTool, append(os.Environ(), launchEnv...))
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, verified
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), verified
	}
	return err.Error(), 1, verified
}

func (g *gatewayConductor) flushRecord(ctx context.Context, key string) error {
	return g.flushRecordPath(ctx, gatewayRecordPath(g.dir, key))
}

func (g *gatewayConductor) flushOutbox(ctx context.Context) error {
	dir := filepath.Join(g.dir, "gateway-tasks")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		keyPath := filepath.Join(dir, entry.Name())
		r, err := readGatewayRecord(keyPath)
		if err != nil {
			return err
		}
		// A sync-route record is owed to the pull timeline only after a pull
		// directive adopted it; a sync call that answered needs no event.
		if r.Route == "sync" && !gatewayAdopted(keyPath) {
			continue
		}
		if r.Result == nil && (r.State != "cancelled" || r.TerminalStateSent) {
			continue
		}
		if r.Result != nil && r.ResultSent && r.TerminalStateSent {
			continue
		}
		if err := g.flushRecordPath(ctx, keyPath); err != nil {
			return err
		}
	}
	return nil
}

func (g *gatewayConductor) flushRecordPath(ctx context.Context, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := readGatewayRecord(path)
	if err != nil {
		return err
	}
	if r.Result != nil && !r.ResultSent {
		if err := g.emit(ctx, r.TaskID, "result", r.Result); err != nil {
			return err
		}
		r.ResultSent = true
		if err := writeGatewayRecord(path, r); err != nil {
			return err
		}
	}
	if !r.TerminalStateSent && (r.Result != nil || r.State == "cancelled") {
		if err := g.emit(ctx, r.TaskID, "state", map[string]any{"state": r.State, "reason": r.Reason}); err != nil {
			return err
		}
		r.TerminalStateSent = true
		if err := writeGatewayRecord(path, r); err != nil {
			return err
		}
	}
	return nil
}

func (g *gatewayConductor) poll(ctx context.Context) error {
	if err := g.flushOutbox(ctx); err != nil {
		return err
	}
	tasks, err := g.client.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		cursorPath := gatewayCursorPath(g.dir, task.ID)
		cursorBytes, readErr := os.ReadFile(cursorPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		cursor := string(cursorBytes)
		resp, err := g.client.GetEvents(ctx, task.ID, cursor, directiveEventLimit)
		if err != nil {
			return err
		}
		for _, ev := range resp.Events {
			if ev.Kind != "directive" {
				continue
			}
			if err := g.apply(ctx, task.ID, ev); err != nil {
				return err
			}
		}
		if resp.Cursor != "" && resp.Cursor != cursor {
			if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(cursorPath, []byte(resp.Cursor), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}
