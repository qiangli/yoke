package dblifecycle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

const wireVersion = "bashy-db-owner-v1"
const replayLimit = 1024
const attachmentTTL = 2 * time.Minute

// SessionInfo is on-disk control metadata only. Database contents and replay
// results stay in the owner process memory. The bearer token is protected by
// owner-only directory and file modes; it is never returned in CLI output.
type SessionInfo struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Address       string `json:"address,omitempty"`
	Token         string `json:"token,omitempty"`
	PID           int    `json:"pid"`
	State         string `json:"state"`
	IdleTTL       string `json:"idle_ttl,omitempty"`
}

// Request is one bounded, authenticated local IPC operation. Holder and Epoch
// are checked against the existing claim ledger by the owner on every access.
type Request struct {
	SchemaVersion string         `json:"schema_version"`
	Token         string         `json:"token"`
	Op            string         `json:"op"`
	Target        dbstore.Target `json:"target,omitempty"`
	Holder        principal.Ref  `json:"holder"`
	Epoch         uint64         `json:"epoch,omitempty"`
	Wait          time.Duration  `json:"wait,omitempty"`
	Out           string         `json:"out,omitempty"`
	In            string         `json:"in,omitempty"`
	Replace       bool           `json:"replace,omitempty"`
	Yes           bool           `json:"yes,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	Attachment    string         `json:"attachment,omitempty"`
}

type Response struct {
	SchemaVersion string      `json:"schema_version"`
	OK            bool        `json:"ok"`
	Op            string      `json:"op"`
	Target        *TargetInfo `json:"target,omitempty"`
	MutationClass string      `json:"mutation_class"`
	Effects       []string    `json:"effects"`
	Result        any         `json:"result,omitempty"`
	Error         *OwnerError `json:"error,omitempty"`
	RequestID     string      `json:"request_id,omitempty"`
}

type OwnerError struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	SafeToRetry bool           `json:"safe_to_retry"`
	Details     map[string]any `json:"details,omitempty"`
}

func sessionDir() (string, error) {
	if p := os.Getenv("BASHY_DB_SESSION_DIR"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "db", "sessions"), nil
}

func infoPath(id string) (string, error) {
	if len(id) < 3 || id[:2] != "s-" {
		return "", fmt.Errorf("%w: invalid session ID", dbstore.ErrInvalidInput)
	}
	for _, c := range id[2:] {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return "", fmt.Errorf("%w: invalid session ID", dbstore.ErrInvalidInput)
		}
	}
	d, err := sessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".json"), nil
}

func readInfo(id string) (SessionInfo, error) {
	p, err := infoPath(id)
	if err != nil {
		return SessionInfo{}, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return SessionInfo{}, fmt.Errorf("%w: session %s", dbstore.ErrNotFound, id)
	}
	if err != nil {
		return SessionInfo{}, err
	}
	var info SessionInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return info, err
	}
	if info.SchemaVersion != wireVersion || info.ID != id {
		return info, fmt.Errorf("%w: session metadata", dbstore.ErrInvalidInput)
	}
	return info, nil
}

func writeInfo(info SessionInfo) error {
	p, err := infoPath(info.ID)
	if err != nil {
		return err
	}
	d := filepath.Dir(p)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(d, ".session-*")
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
	return os.Rename(f.Name(), p)
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ReadSession distinguishes an ended owner from a never-held ID. It does not
// reveal the authentication token.
func ReadSession(id string) (SessionInfo, error) {
	i, err := readInfo(id)
	if err != nil {
		return i, err
	}
	if i.State == "running" {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		conn, err := dialOwner(ctx, i.Address)
		cancel()
		if err != nil {
			i.State = "ended"
			i.Address = ""
		} else {
			_ = conn.Close()
		}
	}
	i.Token = ""
	return i, nil
}

func ListSessions() ([]SessionInfo, error) {
	d, err := sessionDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if os.IsNotExist(err) {
		return []SessionInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-5]
		i, err := ReadSession(id)
		if err == nil {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func Call(ctx context.Context, session string, req Request) (Response, error) {
	info, err := readInfo(session)
	if err != nil {
		return Response{}, err
	}
	if info.State != "running" {
		return Response{}, fmt.Errorf("%w: session %s", dbstore.ErrLifetimeEnded, session)
	}
	conn, err := dialOwner(ctx, info.Address)
	if err != nil {
		return Response{}, fmt.Errorf("%w: session %s owner unavailable: %v", dbstore.ErrLifetimeEnded, session, err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	req.SchemaVersion, req.Token = wireVersion, info.Token
	if req.Holder.Episode == "" {
		req.Holder = coord.Self()
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return Response{}, err
	}
	return res, nil
}

type replay struct {
	response Response
	digest   [32]byte
}
type attachment struct {
	holder  principal.Ref
	expires time.Time
}
type owner struct {
	mem          *dbstore.MemSpace
	info         SessionInfo
	ttl          time.Duration
	listener     net.Listener
	mu           sync.Mutex
	mutationGate dbstore.Gate
	active       int
	last         time.Time
	stopping     bool
	replay       map[string]replay
	order        []string
	holdMu       sync.Mutex
	holds        map[string]*Hold
	admissionMu  sync.Mutex
	admissions   map[string]*semaphore.Weighted
	attachments  map[string]attachment
}

// Serve starts the explicit owner in this process. ready is an owner-only
// rendezvous file passed by the parent. A caller disconnect cancels only its
// own request; it never closes the owner or another client's memory.
func Serve(ctx context.Context, ready string, idleTTL time.Duration) error {
	mem, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		return err
	}
	defer mem.Close()
	ln, address, err := listenOwner(mem.ID())
	if err != nil {
		return err
	}
	defer ln.Close()
	defer removeOwnerAddress(address)
	token, err := ownerToken()
	if err != nil {
		return err
	}
	o := &owner{mem: mem, listener: ln, ttl: idleTTL, last: time.Now(), replay: map[string]replay{}, holds: map[string]*Hold{}, mutationGate: dbstore.NewGate(), admissions: map[string]*semaphore.Weighted{}, attachments: map[string]attachment{}}
	o.info = SessionInfo{SchemaVersion: wireVersion, ID: mem.ID(), Address: address, Token: token, PID: os.Getpid(), State: "running", IdleTTL: idleTTL.String()}
	if err := writeInfo(o.info); err != nil {
		return err
	}
	if ready != "" {
		if err := os.WriteFile(ready, []byte(mem.ID()), 0o600); err != nil {
			return err
		}
	}
	if idleTTL > 0 {
		go o.expiry(ctx)
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || o.isStopping() {
				break
			}
			continue
		}
		go o.handle(conn)
	}
	o.cleanupHolds()
	o.mu.Lock()
	o.info.State = "ended"
	o.info.Address = ""
	o.info.Token = ""
	o.mu.Unlock()
	return writeInfo(o.info)
}

func (o *owner) cleanupHolds() {
	o.holdMu.Lock()
	defer o.holdMu.Unlock()
	for id, h := range o.holds {
		_ = h.CloseBackend()
		_ = h.claim.release()
		delete(o.holds, id)
	}
}

// admit coordinates operations on one target. Readers share admission;
// destructive changes and hold transitions wait for them to finish. This is
// in-process request coordination, not an alternative resource claim.
func (o *owner) admit(ctx context.Context, id string, exclusive bool, wait time.Duration) (func(), error) {
	o.admissionMu.Lock()
	sem := o.admissions[id]
	if sem == nil {
		sem = semaphore.NewWeighted(1024)
		o.admissions[id] = sem
	}
	o.admissionMu.Unlock()
	n := int64(1)
	if exclusive {
		n = 1024
	}
	if wait <= 0 {
		wait = dbstore.DefaultWait
	}
	bounded, cancel := context.WithTimeout(ctx, wait)
	if err := sem.Acquire(bounded, n); err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, dbstore.FromContext(ctx.Err())
		}
		return nil, fmt.Errorf("%w: target admission wait expired", dbstore.ErrBusy)
	}
	cancel()
	return func() { sem.Release(n) }, nil
}

func (o *owner) isStopping() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.stopping }
func (o *owner) expiry(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.mu.Lock()
			o.purgeAttachmentsLocked()
			expired := o.active == 0 && len(o.attachments) == 0 && time.Since(o.last) > o.ttl
			if expired {
				o.stopping = true
			}
			o.mu.Unlock()
			if expired {
				_ = o.listener.Close()
				return
			}
		}
	}
}

func (o *owner) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&req); err != nil {
		return
	}
	if req.SchemaVersion != wireVersion || subtle.ConstantTimeCompare([]byte(req.Token), []byte(o.info.Token)) != 1 {
		return
	}
	o.mu.Lock()
	o.active++
	o.last = time.Now()
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.active--; o.last = time.Now(); o.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, conn); cancel() }()
	res := o.executeReplay(ctx, req)
	_ = json.NewEncoder(conn).Encode(res)
}

func (o *owner) executeReplay(ctx context.Context, req Request) Response {
	if req.Attachment != "" {
		o.mu.Lock()
		o.purgeAttachmentsLocked()
		a, ok := o.attachments[req.Attachment]
		if ok && a.holder.Episode == req.Holder.Episode && a.holder.Name == req.Holder.Name {
			a.expires = time.Now().Add(attachmentTTL)
			o.attachments[req.Attachment] = a
		} else {
			ok = false
		}
		o.mu.Unlock()
		if !ok {
			return failResponse(Response{Op: req.Op, MutationClass: Spec(req.Op, req.Target.Mode).MutationClass, Effects: Spec(req.Op, req.Target.Mode).Effects}, fmt.Errorf("%w: attachment expired or belongs to another client", dbstore.ErrInvalidInput))
		}
	}
	mutating := req.Op == "create" || req.Op == "drop" || req.Op == "restore" || req.Op == "hold" || req.Op == "release"
	if !mutating {
		return o.execute(ctx, req)
	}
	leave, err := o.mutationGate.Enter(ctx, req.Wait)
	if err != nil {
		return failResponse(Response{Op: req.Op, MutationClass: Spec(req.Op, req.Target.Mode).MutationClass, Effects: Spec(req.Op, req.Target.Mode).Effects}, err)
	}
	defer leave()
	if req.RequestID == "" {
		return o.execute(ctx, req)
	}
	key := req.Holder.Episode + "\x00" + req.RequestID
	copyReq := req
	copyReq.Token = ""
	copyReq.RequestID = ""
	b, _ := json.Marshal(copyReq)
	digest := sha256.Sum256(b)
	if old, ok := o.replay[key]; ok {
		if old.digest != digest {
			return failResponse(Response{Op: req.Op, MutationClass: "write", Effects: []string{"local.write"}}, fmt.Errorf("%w: request-id %q reused with different arguments", dbstore.ErrConflict, req.RequestID))
		}
		return old.response
	}
	res := o.execute(ctx, req)
	if res.OK {
		o.replay[key] = replay{response: res, digest: digest}
		o.order = append(o.order, key)
		if len(o.order) > replayLimit {
			delete(o.replay, o.order[0])
			o.order = o.order[1:]
		}
	}
	return res
}

func (o *owner) execute(ctx context.Context, req Request) Response {
	spec := Spec(req.Op, req.Target.Mode)
	res := Response{SchemaVersion: "bashy-db-result-v1", OK: true, Op: req.Op, MutationClass: spec.MutationClass, Effects: spec.Effects, RequestID: req.RequestID}
	if req.Attachment != "" {
		o.mu.Lock()
		o.purgeAttachmentsLocked()
		entry, ok := o.attachments[req.Attachment]
		if ok && entry.holder.Episode == req.Holder.Episode && entry.holder.Name == req.Holder.Name {
			entry.expires = time.Now().Add(attachmentTTL)
			o.attachments[req.Attachment] = entry
		} else {
			ok = false
		}
		o.mu.Unlock()
		if !ok {
			return failResponse(res, fmt.Errorf("%w: attachment expired or belongs to another client", dbstore.ErrInvalidInput))
		}
	}
	if req.Op == "attach" {
		attachmentID, err := randomToken()
		if err != nil {
			return failResponse(res, err)
		}
		o.mu.Lock()
		o.purgeAttachmentsLocked()
		o.attachments[attachmentID] = attachment{holder: req.Holder, expires: time.Now().Add(attachmentTTL)}
		o.mu.Unlock()
		res.Result = map[string]any{"session": o.info.ID, "attachment_id": attachmentID, "attachment_ttl": attachmentTTL.String(), "lifetime": "owner"}
		return res
	}
	if req.Op == "detach" {
		if req.Attachment == "" {
			return failResponse(res, fmt.Errorf("%w: attachment ID required", dbstore.ErrInvalidInput))
		}
		o.mu.Lock()
		delete(o.attachments, req.Attachment)
		o.mu.Unlock()
		res.Result = map[string]any{"session": o.info.ID, "detached": true}
		return res
	}
	if req.Op == "status" {
		o.mu.Lock()
		o.purgeAttachmentsLocked()
		active := o.active
		attached := len(o.attachments)
		o.mu.Unlock()
		res.Result = map[string]any{"session": o.info.ID, "active_requests": active - 1, "attached_clients": attached, "databases": o.memoryNames(), "lifetime": "owner"}
		return res
	}
	if req.Op == "stop" {
		o.mu.Lock()
		o.purgeAttachmentsLocked()
		attached := len(o.attachments)
		busy := o.active > 1 || attached > 0
		if !busy || req.Yes {
			o.stopping = true
		}
		o.mu.Unlock()
		if busy && !req.Yes {
			return failResponse(res, fmt.Errorf("%w: session has active requests or %d attached clients", dbstore.ErrBusy, attached))
		}
		res.Result = map[string]any{"session": o.info.ID, "databases_ended": o.memoryNames()}
		go func() { time.Sleep(20 * time.Millisecond); _ = o.listener.Close() }()
		return res
	}
	if req.Op == "hold" || req.Op == "release" {
		return o.holdRequest(ctx, req, res)
	}
	if req.Target.Mode == dbstore.Memory && req.Target.Session != o.info.ID {
		return failResponse(res, fmt.Errorf("%w: session %s", dbstore.ErrLifetimeEnded, req.Target.Session))
	}
	s := Service{Mem: o.mem, Holder: req.Holder}
	var id dbstore.Identity
	var err error
	id, err = dbstore.Resolve(req.Target)
	if err != nil {
		return failResponse(res, err)
	}
	view := ViewTarget(id, "")
	res.Target = &view
	leave, err := o.admit(ctx, id.ID, req.Op == "create" || req.Op == "drop" || req.Op == "restore", req.Wait)
	if err != nil {
		return failResponse(res, err)
	}
	defer leave()
	o.holdMu.Lock()
	held := o.holds[id.ID]
	if held != nil {
		if held.claim.holder.Episode != req.Holder.Episode || held.claim.holder.Name != req.Holder.Name {
			o.holdMu.Unlock()
			return failResponse(res, &coord.Conflict{Claim: held.claim.claim})
		}
		if req.Epoch == 0 || req.Epoch != held.Epoch {
			o.holdMu.Unlock()
			return failResponse(res, coord.ErrFenced)
		}
		if err := held.Check(ctx, req.Epoch); err != nil {
			_ = held.CloseBackend()
			_ = held.claim.release()
			delete(o.holds, id.ID)
			o.holdMu.Unlock()
			return failResponse(res, err)
		}
	}
	o.holdMu.Unlock()
	switch req.Op {
	case "create":
		if held != nil {
			return failResponse(res, fmt.Errorf("%w: target is already held", dbstore.ErrExists))
		}
		id, err = s.Create(ctx, req.Target, req.Wait)
	case "show":
		if held != nil {
			res.Result, err = showHandle(held.handle)
		} else {
			id, res.Result, err = s.Show(ctx, req.Target, req.Wait)
		}
	case "check":
		var level string
		if held != nil {
			level, err = inspectHandle(ctx, held.handle)
		} else {
			id, level, err = s.Inspect(ctx, req.Target, req.Wait)
		}
		if err == nil {
			res.Result = map[string]any{"check": level, "id": id.ID}
		}
	case "backup":
		if held != nil {
			err = backupHandle(ctx, held.handle, req.Out)
		} else {
			id, err = s.Backup(ctx, req.Target, req.Out, req.Wait)
		}
	case "restore":
		if !req.Yes {
			err = fmt.Errorf("%w: --yes required", dbstore.ErrInvalidInput)
		} else {
			if held != nil {
				err = held.CloseBackend()
				if err == nil {
					id, err = s.Restore(ctx, req.Target, req.In, req.Replace, req.Wait)
				}
				o.finishHold(id.ID, held)
			} else {
				id, err = s.Restore(ctx, req.Target, req.In, req.Replace, req.Wait)
			}
		}
	case "drop":
		if !req.Yes {
			err = fmt.Errorf("%w: --yes required", dbstore.ErrInvalidInput)
		} else {
			if held != nil {
				err = held.CloseBackend()
				if err == nil {
					id, err = s.Drop(ctx, req.Target, req.Wait)
				}
				o.finishHold(id.ID, held)
			} else {
				id, err = s.Drop(ctx, req.Target, req.Wait)
			}
		}
	default:
		err = fmt.Errorf("%w: owner operation %s", dbstore.ErrUnsupported, req.Op)
	}
	if err != nil {
		return failResponse(res, err)
	}
	view = ViewTarget(id, "")
	res.Target = &view
	if res.Result == nil {
		res.Result = map[string]any{"id": id.ID}
	}
	return res
}

func (o *owner) purgeAttachmentsLocked() {
	now := time.Now()
	for id, a := range o.attachments {
		if !a.expires.After(now) {
			delete(o.attachments, id)
		}
	}
}

func (o *owner) memoryNames() []string {
	var names []string
	for _, engine := range []string{"sqlite", "bbolt", "bleve", "bonsai"} {
		for _, name := range o.mem.Names(engine) {
			names = append(names, "db:"+engine+":mem:"+o.info.ID+"/"+name)
		}
	}
	sort.Strings(names)
	return names
}

func (o *owner) finishHold(id string, h *Hold) {
	o.holdMu.Lock()
	delete(o.holds, id)
	o.holdMu.Unlock()
	_ = h.claim.release()
}

func (o *owner) holdRequest(ctx context.Context, req Request, res Response) Response {
	if req.Target.Mode == dbstore.Memory && req.Target.Session != o.info.ID {
		return failResponse(res, fmt.Errorf("%w: session %s", dbstore.ErrLifetimeEnded, req.Target.Session))
	}
	id, err := dbstore.Resolve(req.Target)
	if err != nil {
		return failResponse(res, err)
	}
	view := ViewTarget(id, "")
	res.Target = &view
	leave, err := o.admit(ctx, id.ID, true, req.Wait)
	if err != nil {
		return failResponse(res, err)
	}
	defer leave()
	o.holdMu.Lock()
	defer o.holdMu.Unlock()
	if h := o.holds[id.ID]; h != nil {
		if h.claim.holder.Episode != req.Holder.Episode || h.claim.holder.Name != req.Holder.Name {
			return failResponse(res, &coord.Conflict{Claim: h.claim.claim})
		}
		presented := req.Epoch
		if req.Op == "hold" && presented == 0 {
			presented = h.Epoch
		}
		if presented != h.Epoch {
			return failResponse(res, coord.ErrFenced)
		}
		if err := h.Check(ctx, presented); err != nil {
			_ = h.CloseBackend()
			_ = h.claim.release()
			delete(o.holds, id.ID)
			return failResponse(res, err)
		}
		if req.Op == "hold" {
			res.Result = map[string]any{"id": id.ID, "epoch": h.Epoch, "nested": true}
			return res
		}
		if err := h.Release(ctx, req.Epoch); err != nil {
			return failResponse(res, err)
		}
		delete(o.holds, id.ID)
		res.Result = map[string]any{"id": id.ID, "released": true}
		return res
	}
	if req.Op == "release" {
		return failResponse(res, fmt.Errorf("%w: no owner hold on %s", dbstore.ErrNotFound, id.ID))
	}
	s := Service{Mem: o.mem, Holder: req.Holder}
	h, err := s.AcquireHold(ctx, id.Target, req.Wait)
	if err != nil {
		return failResponse(res, err)
	}
	o.holds[id.ID] = h
	res.Result = map[string]any{"id": id.ID, "epoch": h.Epoch, "nested": false, "scope": "database"}
	return res
}

func failResponse(res Response, err error) Response {
	res.OK = false
	res.SchemaVersion = "bashy-db-error-v1"
	code := dbstore.Code(err)
	var conflict *coord.Conflict
	if errors.As(err, &conflict) {
		code = "claim_held"
	}
	if errors.Is(err, coord.ErrFenced) {
		code = "stale_epoch"
	}
	body := &OwnerError{Code: code, Message: err.Error(), Retryable: code == "busy" || code == "conflict" || code == "timeout" || code == "claim_held", SafeToRetry: res.MutationClass == "read" || code == "not_found" || code == "exists" || code == "claim_held" || code == "stale_epoch" || code == "invalid_input" || code == "unsupported"}
	if conflict != nil {
		b, _ := conflict.JSON()
		var claim any
		if json.Unmarshal(b, &claim) == nil {
			body.Details = map[string]any{"claim": claim, "scope": "database"}
		}
	}
	res.Error = body
	return res
}
