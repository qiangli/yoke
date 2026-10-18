// Package kbmemory is the Sprint 453 memory collection layer behind
// `bashy kb memory` (story 564cbba7c9fd): an explicitly initialized,
// opt-in collection of kb-shaped memory records stored in a `db` document
// target, in either storage mode.
//
// It sits beside pkg/recall and composes the db library directly (contract
// §10.1): pkg/dblifecycle for admission, claims, owner sessions and
// request-id replay, pkg/dbdoc for revisioned documents. add and update are
// registered category operations (memory.add, memory.update) whose input is
// the logical request; the record ID and timestamps are generated inside
// execution, so the owner's existing replay digest covers exactly the
// logical operation and a retry returns the committed outcome. db never imports
// this package and pkg/kb stays a leaf; the record vocabulary (type, status,
// source provenance) is kb's own, read from pkg/kb rather than redefined.
//
// Nothing is implicit. There is no default collection and no persisted
// "current" selection (contract invariant 2): every verb names its target.
// `init` is the only verb that creates anything, and it reports where.
// Records are written `candidate` (I-kb-4) and nothing is ever captured
// automatically. Supersede and forget consume the one lifecycle semantics,
// pkg/kblife (284 M6): an interval is closed, a tombstone added, and nothing
// is deleted.
package kbmemory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kblife"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

const (
	ResultSchema = "bashy-kb-memory-result-v1"
	ErrorSchema  = "bashy-kb-memory-error-v1"
	// Collection is the document collection a memory target holds.
	Collection = "memory"
	// Engine is the only engine with the document category today.
	Engine = "sqlite"
	// MaxBody bounds one record body, under dbdoc.MaxDocBytes so the
	// metadata always fits.
	MaxBody = 1 << 20
)

// Verbs, in the order `kb memory` documents them.
var Verbs = []string{"init", "select", "add", "inspect", "update", "supersede", "forget"}

// Scope mirrors kb's page scope.
type Scope struct {
	Repos []string `json:"repos,omitempty"`
	OS    string   `json:"os,omitempty"`
}

// Source mirrors kb's page provenance plus the caller-given source ref.
type Source struct {
	Tool          string `json:"tool,omitempty"`
	Host          string `json:"host,omitempty"`
	Episode       string `json:"episode,omitempty"`
	ParentEpisode string `json:"parent_episode,omitempty"`
	Ref           string `json:"ref,omitempty"`
}

// Record is one memory document. Its ID is stable for its lifetime.
type Record struct {
	ID      string   `json:"id"`
	Form    string   `json:"form"`
	Type    string   `json:"type"`
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body"`
	Tags    []string `json:"tags"`
	Scope   Scope    `json:"scope"`
	Status  string   `json:"status"`
	Source  Source   `json:"source"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
	// Lifecycle (pkg/kblife), additive: RFC 3339 interval bounds, the
	// supersede links, and the forget tombstone. Empty means open/none.
	ValidFrom    string            `json:"valid_from,omitempty"`
	ValidTo      string            `json:"valid_to,omitempty"`
	Supersedes   string            `json:"supersedes,omitempty"`
	SupersededBy string            `json:"superseded_by,omitempty"`
	Tombstone    *kblife.Tombstone `json:"tombstone,omitempty"`
}

// Validity is the record's interval as kblife reads it.
func (r Record) Validity() kblife.Validity {
	parse := func(s string) time.Time {
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	return kblife.Validity{ValidFrom: parse(r.ValidFrom), ValidTo: parse(r.ValidTo)}
}

// State resolves the record at asOf; retrieval shows only kblife.Visible.
func (r Record) State(asOf time.Time) kblife.State {
	return kblife.Resolve(r.Validity(), r.SupersededBy, r.Tombstone, asOf)
}

// Request is one typed memory operation. CLI and MCP both build it from the
// same flags. Optional update fields are pointers: nil means unchanged.
type Request struct {
	Verb       string
	Target     string // S11 db locator for sqlite:file:PATH or sqlite:mem:SESSION/NAME
	Session    string // explicit owner session for a persistent target
	Dir        string // resolves a relative persistent path
	ID         string
	Type       *string
	Title      *string
	Body       *string
	Tags       *[]string
	Repos      *[]string
	OS         *string
	Episode    string
	SourceRef  string
	IfRevision int64
	// supersede: the replacement record and its revision precondition.
	Replacement         string
	ReplacementRevision int64
	// forget: why (required).
	Reason    string
	RequestID string
	Wait      time.Duration
}

// clock stamps created/updated inside operation execution (tests pin it).
var clock = time.Now

// ErrorBody is the bashy-db-error-v1 error object, reused unchanged.
type ErrorBody struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	SafeToRetry bool           `json:"safe_to_retry"`
	Details     map[string]any `json:"details,omitempty"`
}

// Envelope is the versioned result or error of every verb.
type Envelope struct {
	SchemaVersion string                  `json:"schema_version"`
	OK            bool                    `json:"ok"`
	Op            string                  `json:"op"`
	Target        *dblifecycle.TargetInfo `json:"target,omitempty"`
	Collection    string                  `json:"collection"`
	MutationClass string                  `json:"mutation_class"`
	RequestID     string                  `json:"request_id,omitempty"`
	Revision      *int64                  `json:"revision,omitempty"`
	Result        any                     `json:"result,omitempty"`
	Error         *ErrorBody              `json:"error,omitempty"`
}

// ExitCode follows the db contract §5.2 table.
func (e Envelope) ExitCode() int {
	if e.Error == nil {
		return 0
	}
	switch e.Error.Code {
	case "invalid_input", "unsupported", "exclusion_unsupported":
		return 2
	case "claim_held", "stale_epoch":
		return 9
	case "timeout":
		return 124
	case "cancelled":
		return 130
	default:
		return 1
	}
}

func mutationClass(verb string) string {
	switch verb {
	case "add", "update", "init", "supersede", "forget":
		return "write"
	}
	return "read"
}

// ownerError carries an owner session's structured refusal unchanged.
type ownerError struct{ body dblifecycle.OwnerError }

func (e *ownerError) Error() string { return e.body.Message }

// Run executes one memory verb and returns its envelope. It never panics on
// caller input and never creates a target outside `init`.
func Run(ctx context.Context, req Request) Envelope {
	env := Envelope{SchemaVersion: ResultSchema, OK: true, Op: "memory." + req.Verb, Collection: Collection, MutationClass: mutationClass(req.Verb), RequestID: req.RequestID}
	t, err := parseTarget(req)
	if err == nil {
		var id dbstore.Identity
		if id, err = dbstore.Resolve(t); err == nil {
			info := dblifecycle.ViewTarget(id, "")
			env.Target = &info
			err = run(ctx, req, t, id, &env)
		}
	}
	if err != nil {
		return fail(env, err)
	}
	return env
}

func fail(env Envelope, err error) Envelope {
	env.SchemaVersion, env.OK, env.Result, env.Revision = ErrorSchema, false, nil, nil
	var oe *ownerError
	if errors.As(err, &oe) {
		b := oe.body
		env.Error = &ErrorBody{Code: b.Code, Message: b.Message, Retryable: b.Retryable, SafeToRetry: b.SafeToRetry, Details: b.Details}
		return env
	}
	code := dbstore.Code(err)
	var conflict *coord.Conflict
	if errors.As(err, &conflict) {
		code = "claim_held"
	}
	if errors.Is(err, coord.ErrFenced) {
		code = "stale_epoch"
	}
	body := &ErrorBody{Code: code, Message: err.Error(),
		Retryable:   code == "busy" || code == "conflict" || code == "timeout" || code == "claim_held",
		SafeToRetry: env.MutationClass == "read" || code == "not_found" || code == "exists" || code == "claim_held" || code == "stale_epoch" || code == "invalid_input" || code == "unsupported"}
	if conflict != nil {
		if b, e := conflict.JSON(); e == nil {
			var claim any
			if json.Unmarshal(b, &claim) == nil {
				body.Details = map[string]any{"claim": claim, "scope": "database"}
			}
		}
	}
	env.Error = body
	return env
}

func parseTarget(req Request) (dbstore.Target, error) {
	if !slices.Contains(Verbs, req.Verb) {
		return dbstore.Target{}, fmt.Errorf("%w: memory verb %q (want %s)", dbstore.ErrInvalidInput, req.Verb, strings.Join(Verbs, "|"))
	}
	if strings.TrimSpace(req.Target) == "" {
		return dbstore.Target{}, fmt.Errorf("%w: an explicit TARGET locator is required (the db locator, e.g. db:sqlite%%3Afile%%3APATH)", dbstore.ErrInvalidInput)
	}
	t, err := dblifecycle.ParseTarget(req.Target)
	if err != nil {
		return t, err
	}
	if t.Engine != Engine {
		return t, fmt.Errorf("%w: memory collections need the document category (engine %s), not %s", dbstore.ErrUnsupported, Engine, t.Engine)
	}
	if t.Mode == dbstore.Memory && req.Session != "" && req.Session != t.Session {
		return t, fmt.Errorf("%w: --session %s does not own target session %s", dbstore.ErrInvalidInput, req.Session, t.Session)
	}
	if t.Mode == dbstore.Persistent && !filepath.IsAbs(t.Path) && req.Dir != "" {
		t.Path = filepath.Join(req.Dir, t.Path)
	}
	return t, nil
}

// session is the owner the call goes through: the memory target's own
// session, or an explicitly given shared owner for a persistent target.
func session(req Request, t dbstore.Target) string {
	if t.Mode == dbstore.Memory {
		return t.Session
	}
	return req.Session
}

// Category operations kbmemory registers on the shared admission path.
const (
	OpAdd       = "memory.add"
	OpUpdate    = "memory.update"
	OpSupersede = "memory.supersede"
	OpForget    = "memory.forget"
)

func init() {
	register(OpAdd, runAdd)
	register(OpUpdate, runUpdate)
	register(OpSupersede, runSupersede)
	register(OpForget, runForget)
}

// opInput is the logical memory request: what the owner's request-id digest
// covers. It carries no generated ID or time. Source is the deliberate
// per-operation provenance (tool, host, episode) the caller stamps.
//
// Replay scope is deliberate: the owner keys a request ID by principal and
// digests this input, provenance included. The same principal retrying the
// same logical request replays the committed outcome; another principal or
// episode is a different request (an add writes its own record, a stale
// update is a revision conflict) — never a replay.
type opInput struct {
	RequestID  string    `json:"request_id,omitempty"`
	ID         string    `json:"id,omitempty"`
	IfRevision int64     `json:"if_revision,omitempty"`
	Type       *string   `json:"type,omitempty"`
	Title      *string   `json:"title,omitempty"`
	Body       *string   `json:"body,omitempty"`
	Tags       *[]string `json:"tags,omitempty"`
	Repos      *[]string `json:"repos,omitempty"`
	OS         *string   `json:"os,omitempty"`
	Source     Source    `json:"source"`
	// Replacement/ReplacementRevision: supersede. Reason: forget.
	Replacement         string `json:"replacement,omitempty"`
	ReplacementRevision int64  `json:"replacement_revision,omitempty"`
	Reason              string `json:"reason,omitempty"`
}

func decodeOp(raw json.RawMessage) (opInput, error) {
	var in opInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: memory input: %v", dbstore.ErrInvalidInput, err)
	}
	return in, nil
}

func register(name string, run func(context.Context, dbstore.Handle, opInput) (dblifecycle.Outcome, error)) {
	dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			_, err := decodeOp(raw)
			return dblifecycle.AccessWrite, err
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return dbdoc.Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := decodeOp(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if a != dblifecycle.AccessWrite {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: %s admitted with the wrong access", dbstore.ErrInvalidInput, name)
			}
			return run(ctx, h, in)
		},
	})
}

// docOp runs one operation (a dbdoc read or a memory write) through the
// shared admission path: the explicit owner session, or the direct claim
// guard for a persistent target.
func docOp(ctx context.Context, req Request, t dbstore.Target, op string, in any, yes bool) (any, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if s := session(req, t); s != "" {
		res, err := dblifecycle.Call(ctx, s, dblifecycle.Request{Op: op, Target: t, Wait: req.Wait, Yes: yes, RequestID: req.RequestID, Input: raw})
		if err != nil {
			return nil, err
		}
		if !res.OK {
			if res.Error == nil {
				return nil, fmt.Errorf("%w: owner refused %s without an error", dbstore.ErrBackend, op)
			}
			return nil, &ownerError{*res.Error}
		}
		return res.Result, nil
	}
	if req.RequestID != "" {
		return nil, fmt.Errorf("%w: --request-id deduplication is an owner-session guarantee; pass --session", dbstore.ErrUnsupported)
	}
	_, out, err := dblifecycle.Service{}.RunCategory(ctx, t, op, raw, yes, req.Wait)
	return out.Result, err
}

// onHandle runs a dbdoc operation on the handle a memory operation was
// admitted with, through dbdoc's registered entry point.
func onHandle(ctx context.Context, h dbstore.Handle, name string, in dbdoc.Input) (any, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	op, a, ok, err := dblifecycle.LookupCategoryOp(name, raw)
	if !ok {
		return nil, fmt.Errorf("%w: db operation %s", dbstore.ErrUnsupported, name)
	}
	if err != nil {
		return nil, err
	}
	out, err := op.Run(ctx, h, raw, a)
	return out.Result, err
}

// decode re-reads an operation result (a typed value on the direct path, a
// decoded JSON map through an owner) into v.
func decode(res any, v any) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func run(ctx context.Context, req Request, t dbstore.Target, id dbstore.Identity, env *Envelope) error {
	if req.Verb != "add" && req.Verb != "update" && hasFields(req) {
		return fmt.Errorf("%w: record fields apply only to add and update", dbstore.ErrInvalidInput)
	}
	takesID := req.Verb == "inspect" || req.Verb == "update" || req.Verb == "supersede" || req.Verb == "forget"
	if takesID != (req.ID != "") {
		return fmt.Errorf("%w: inspect, update, supersede and forget take exactly one record ID; other verbs take none", dbstore.ErrInvalidInput)
	}
	if (req.Verb == "inspect" || !takesID) && req.IfRevision != 0 {
		return fmt.Errorf("%w: --if-revision applies only to update, supersede and forget", dbstore.ErrInvalidInput)
	}
	if req.Verb != "supersede" && (req.Replacement != "" || req.ReplacementRevision != 0) {
		return fmt.Errorf("%w: --replacement applies only to supersede", dbstore.ErrInvalidInput)
	}
	if req.Verb != "forget" && req.Reason != "" {
		return fmt.Errorf("%w: --reason applies only to forget", dbstore.ErrInvalidInput)
	}
	switch req.Verb {
	case "init":
		return initCollection(ctx, req, t, id, env)
	case "select":
		return selectCollection(ctx, req, t, env)
	case "add":
		return add(ctx, req, t, env)
	case "inspect":
		return inspect(ctx, req, t, env)
	case "supersede":
		return supersede(ctx, req, t, env)
	case "forget":
		return forget(ctx, req, t, env)
	default:
		return update(ctx, req, t, env)
	}
}

func hasFields(req Request) bool {
	return req.Type != nil || req.Title != nil || req.Body != nil || req.Tags != nil || req.Repos != nil || req.OS != nil || req.Episode != "" || req.SourceRef != ""
}

// initCollection is the one creating verb: an explicit db create plus the
// memory collection, reporting each location it made.
func initCollection(ctx context.Context, req Request, t dbstore.Target, id dbstore.Identity, env *Envelope) error {
	// init is two mutations (create, then the collection); a replay of the
	// pair is not one owner request, so refuse rather than half-replay it.
	if req.RequestID != "" {
		return fmt.Errorf("%w: --request-id is not supported for init (create + collection is not one replayable request)", dbstore.ErrUnsupported)
	}
	if s := session(req, t); s != "" {
		res, err := dblifecycle.Call(ctx, s, dblifecycle.Request{Op: "create", Target: t, Wait: req.Wait})
		if err != nil {
			return err
		}
		if !res.OK {
			if res.Error == nil {
				return fmt.Errorf("%w: owner refused create without an error", dbstore.ErrBackend)
			}
			return &ownerError{*res.Error}
		}
	} else if _, err := (dblifecycle.Service{}).Create(ctx, t, req.Wait); err != nil {
		return err
	}
	// The collection is created only in a database this call just created.
	if _, err := docOp(ctx, req, t, dbdoc.OpCollectionCreate, dbdoc.Input{Collection: Collection}, true); err != nil {
		return fmt.Errorf("%w (database %s was created; the memory collection was not)", err, id.ID)
	}
	store := map[string]any{"role": "documents", "id": id.ID, "engine": id.Engine, "mode": id.Mode, "collection": Collection}
	if id.Mode == dbstore.Persistent {
		store["path"] = id.Path
	} else {
		store["session"] = id.Session
		store["lifetime"] = "owner"
	}
	env.Result = map[string]any{"initialized": true, "stores": []any{store}}
	return nil
}

// selectCollection verifies, read-only, that the target holds an
// initialized memory collection. It records no selection anywhere.
func selectCollection(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	res, err := docOp(ctx, req, t, dbdoc.OpCollectionList, dbdoc.Input{Limit: dbdoc.MaxLimit}, false)
	if err != nil {
		return err
	}
	var list struct {
		Collections []struct {
			Name      string `json:"name"`
			Documents int64  `json:"documents"`
		} `json:"collections"`
	}
	if err := decode(res, &list); err != nil {
		return err
	}
	for _, c := range list.Collections {
		if c.Name == Collection {
			env.Result = map[string]any{"selected": true, "records": c.Documents}
			return nil
		}
	}
	return fmt.Errorf("%w: target has no memory collection; run `kb memory init` on it explicitly", dbstore.ErrNotFound)
}

func stamp() string { return clock().UTC().Format(time.RFC3339) }

func checkType(t string) error {
	if !kb.ValidType(t) {
		return fmt.Errorf("%w: type %q (lesson|gotcha|runbook|decision|fact)", dbstore.ErrInvalidInput, t)
	}
	return nil
}

func checkBody(b string) error {
	if strings.TrimSpace(b) == "" {
		return fmt.Errorf("%w: a record needs a body (--body, or -f FILE / -f - for stdin)", dbstore.ErrInvalidInput)
	}
	if len(b) > MaxBody {
		return fmt.Errorf("%w: body is %d bytes, over %d", dbstore.ErrLimitExceeded, len(b), MaxBody)
	}
	return nil
}

func clean(list []string) []string {
	out := []string{}
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func trim(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

func cleanP(p *[]string) *[]string {
	if p == nil {
		return nil
	}
	v := clean(*p)
	return &v
}

// logical is req's normalized operation input, so equivalent retries
// produce identical owner requests.
func logical(req Request) opInput {
	return opInput{RequestID: req.RequestID, ID: req.ID, IfRevision: req.IfRevision, Type: trim(req.Type), Title: trim(req.Title), Body: trim(req.Body),
		Tags: cleanP(req.Tags), Repos: cleanP(req.Repos), OS: trim(req.OS), Source: Source{Ref: strings.TrimSpace(req.SourceRef)}}
}

func add(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	in := logical(req)
	// Provenance per operation, from the same identity sources kb stamps.
	// It is part of the request on purpose: the principal that asked.
	in.Source.Tool, in.Source.Host, in.Source.Episode = kb.ToolID(), kb.HostID(), strings.TrimSpace(req.Episode)
	if in.Source.Episode == "" {
		in.Source.Episode = kb.EpisodeID()
	}
	if in.Source.Episode != "" && in.Source.Episode == kb.EpisodeID() {
		in.Source.ParentEpisode = kb.ParentEpisodeID()
	}
	res, err := docOp(ctx, req, t, OpAdd, in, false)
	if err != nil {
		return err
	}
	return finish(env, res)
}

// finish reports the record and the document revision the write produced.
func finish(env *Envelope, res any) error {
	var w struct {
		Revision int64          `json:"revision"`
		Result   map[string]any `json:"result"`
	}
	if err := decode(res, &w); err != nil {
		return err
	}
	env.Revision = &w.Revision
	env.Result = w.Result
	return nil
}

// runAdd is memory.add: it mints the random ID and time inside execution,
// so the owner's replay of a request ID covers only the logical input and
// its provenance. There is no other replay state.
func runAdd(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.ID != "" || in.IfRevision != 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: add takes no record ID or revision", dbstore.ErrInvalidInput)
	}
	r := Record{Form: kb.FormNote, Type: kb.TypeLesson, Tags: []string{}, Status: kb.StatusCandidate, Source: in.Source}
	if in.Type != nil {
		r.Type = *in.Type
	}
	if in.Body != nil {
		r.Body = *in.Body
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Tags != nil {
		r.Tags = *in.Tags
	}
	if in.Repos != nil {
		r.Scope.Repos = *in.Repos
	}
	if in.OS != nil {
		r.Scope.OS = *in.OS
	}
	if err := checkType(r.Type); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := checkBody(r.Body); err != nil {
		return dblifecycle.Outcome{}, err
	}
	id, err := newID()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	r.ID = id
	r.Created = stamp()
	r.Updated = r.Created
	doc, err := json.Marshal(r)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	res, err := onHandle(ctx, h, dbdoc.OpPut, dbdoc.Input{Collection: Collection, ID: r.ID, Doc: doc, IfAbsent: true})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Revision int64 `json:"revision"`
	}
	if err := decode(res, &w); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return outcome(w.Revision, map[string]any{"record": r}), nil
}

func outcome(rev int64, result map[string]any) dblifecycle.Outcome {
	return dblifecycle.Outcome{Result: map[string]any{"revision": rev, "result": result}}
}

func getOn(ctx context.Context, h dbstore.Handle, id string) (Record, int64, error) {
	res, err := onHandle(ctx, h, dbdoc.OpGet, dbdoc.Input{Collection: Collection, ID: id})
	if err != nil {
		return Record{}, 0, err
	}
	return decodeDoc(res, id)
}

func decodeDoc(res any, id string) (Record, int64, error) {
	var d struct {
		Revision int64  `json:"revision"`
		Doc      Record `json:"doc"`
	}
	if err := decode(res, &d); err != nil {
		return Record{}, 0, fmt.Errorf("%w: memory record %q: %v", dbstore.ErrIntegrity, id, err)
	}
	return d.Doc, d.Revision, nil
}

func inspect(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	res, err := docOp(ctx, req, t, dbdoc.OpGet, dbdoc.Input{Collection: Collection, ID: req.ID}, false)
	if err != nil {
		return err
	}
	r, rev, err := decodeDoc(res, req.ID)
	if err != nil {
		return err
	}
	env.Revision = &rev
	env.Result = map[string]any{"record": r, "state": r.State(clock())}
	return nil
}

// update rewrites the fields given, under a required revision precondition,
// so two agents updating one record get `conflict` instead of a lost update.
// Status is not writable here: promotion stays `kb validate --from-gate`.
func update(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	if req.IfRevision < 1 {
		return fmt.Errorf("%w: update requires --if-revision N (the revision inspect reported)", dbstore.ErrInvalidInput)
	}
	if req.Episode != "" {
		return fmt.Errorf("%w: a record's episode is its provenance and is not updatable", dbstore.ErrInvalidInput)
	}
	res, err := docOp(ctx, req, t, OpUpdate, logical(req), false)
	if err != nil {
		return err
	}
	return finish(env, res)
}

// runUpdate is memory.update: the `updated` time is stamped here, inside
// execution, so a retried request is the same owner request and the owner
// replays its committed outcome even after later updates moved the record.
func runUpdate(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.IfRevision < 1 || in.ID == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: update needs a record ID and --if-revision", dbstore.ErrInvalidInput)
	}
	if in.Source.Tool != "" || in.Source.Host != "" || in.Source.Episode != "" || in.Source.ParentEpisode != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: a record's provenance is not updatable", dbstore.ErrInvalidInput)
	}
	patch := map[string]any{}
	if in.Type != nil {
		if err := checkType(*in.Type); err != nil {
			return dblifecycle.Outcome{}, err
		}
		patch["type"] = *in.Type
	}
	if in.Body != nil {
		if err := checkBody(*in.Body); err != nil {
			return dblifecycle.Outcome{}, err
		}
		patch["body"] = *in.Body
	}
	if in.Title != nil {
		patch["title"] = *in.Title
	}
	if in.Tags != nil {
		patch["tags"] = *in.Tags
	}
	scope := map[string]any{}
	if in.Repos != nil {
		scope["repos"] = *in.Repos
	}
	if in.OS != nil {
		scope["os"] = *in.OS
	}
	if len(scope) > 0 {
		patch["scope"] = scope
	}
	if in.Source.Ref != "" {
		patch["source"] = map[string]any{"ref": in.Source.Ref}
	}
	if len(patch) == 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: update needs at least one field to change", dbstore.ErrInvalidInput)
	}
	patch["updated"] = stamp()
	raw, err := json.Marshal(patch)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	rev := in.IfRevision
	res, err := onHandle(ctx, h, dbdoc.OpPatch, dbdoc.Input{Collection: Collection, ID: in.ID, Patch: raw, IfRevision: &rev})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Revision int64 `json:"revision"`
	}
	if err := decode(res, &w); err != nil {
		return dblifecycle.Outcome{}, err
	}
	// Report the record as stored, read back on the same handle. A
	// concurrent direct-path writer may already have moved it on; then only
	// the written revision is reported, never another revision's record. The
	// update has committed: a failed read-back is reported as that, not as
	// an error a caller would retry.
	r, readRev, err := getOn(ctx, h, in.ID)
	switch {
	case err != nil:
		return outcome(w.Revision, map[string]any{"id": in.ID, "committed": true, "readback_error": err.Error()}), nil
	case readRev != w.Revision:
		return outcome(w.Revision, map[string]any{"id": in.ID, "record_revision": readRev}), nil
	}
	return outcome(w.Revision, map[string]any{"record": r}), nil
}

// supersede replaces OLD by an existing replacement record: both revisions
// are preconditions, and both records change in one atomic dbdoc batch, so
// a stale side or a failed check leaves neither half-applied.
func supersede(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	if req.IfRevision < 1 || req.Replacement == "" || req.ReplacementRevision < 1 {
		return fmt.Errorf("%w: supersede requires --if-revision N, --replacement ID and --replacement-revision M", dbstore.ErrInvalidInput)
	}
	in := opInput{RequestID: req.RequestID, ID: req.ID, IfRevision: req.IfRevision, Replacement: strings.TrimSpace(req.Replacement), ReplacementRevision: req.ReplacementRevision}
	res, err := docOp(ctx, req, t, OpSupersede, in, false)
	if err != nil {
		return err
	}
	return finish(env, res)
}

// runSupersede is memory.supersede. The instant is taken inside execution,
// and kblife.Supersede is the one interval rule.
func runSupersede(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.ID == "" || in.IfRevision < 1 || in.Replacement == "" || in.ReplacementRevision < 1 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: supersede needs both record IDs and revisions", dbstore.ErrInvalidInput)
	}
	if in.ID == in.Replacement {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: a record cannot supersede itself", dbstore.ErrInvalidInput)
	}
	old, oldRev, err := getOn(ctx, h, in.ID)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	rep, repRev, err := getOn(ctx, h, in.Replacement)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if oldRev != in.IfRevision || repRev != in.ReplacementRevision {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: revision mismatch: %s expected %d actual %d, %s expected %d actual %d",
			dbstore.ErrConflict, in.ID, in.IfRevision, oldRev, in.Replacement, in.ReplacementRevision, repRev)
	}
	at := clock().UTC().Truncate(time.Second)
	for _, r := range []Record{old, rep} {
		if s := r.State(at); !kblife.Visible(s) {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: record %s is %s, not valid", dbstore.ErrInvalidInput, r.ID, s)
		}
	}
	if rep.Supersedes != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: record %s already supersedes %s", dbstore.ErrInvalidInput, rep.ID, rep.Supersedes)
	}
	ov, rv := old.Validity(), rep.Validity()
	if err := kblife.Supersede(&ov, &rv, at); err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	now := at.Format(time.RFC3339)
	old.ValidTo, old.SupersededBy, old.Status, old.Updated = now, rep.ID, kb.StatusSuperseded, now
	rep.ValidFrom, rep.Supersedes, rep.Updated = now, old.ID, now
	oldPatch, err := json.Marshal(map[string]any{"valid_to": old.ValidTo, "superseded_by": old.SupersededBy, "status": old.Status, "updated": now})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	repPatch, err := json.Marshal(map[string]any{"valid_from": rep.ValidFrom, "supersedes": rep.Supersedes, "updated": now})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	or, rr := in.IfRevision, in.ReplacementRevision
	res, err := onHandle(ctx, h, dbdoc.OpBatch, dbdoc.Input{Collection: Collection, Ops: []dbdoc.BatchOp{
		{Op: "patch", ID: old.ID, Patch: oldPatch, IfRevision: &or},
		{Op: "patch", ID: rep.ID, Patch: repPatch, IfRevision: &rr},
	}})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Results []struct {
			Revision int64 `json:"revision"`
		} `json:"results"`
	}
	if err := decode(res, &w); err != nil || len(w.Results) != 2 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: supersede batch result: %v", dbstore.ErrIntegrity, err)
	}
	return outcome(w.Results[0].Revision, map[string]any{"record": old, "replacement": rep, "replacement_revision": w.Results[1].Revision}), nil
}

// forget hides a record behind a kblife tombstone under a revision
// precondition. The record and its provenance are kept for audit.
func forget(ctx context.Context, req Request, t dbstore.Target, env *Envelope) error {
	if req.IfRevision < 1 || strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: forget requires --if-revision N and --reason R", dbstore.ErrInvalidInput)
	}
	// The forgetting principal is part of the request, as add's provenance is.
	in := opInput{RequestID: req.RequestID, ID: req.ID, IfRevision: req.IfRevision, Reason: strings.TrimSpace(req.Reason),
		Source: Source{Tool: kb.ToolID(), Episode: kb.EpisodeID()}}
	res, err := docOp(ctx, req, t, OpForget, in, false)
	if err != nil {
		return err
	}
	return finish(env, res)
}

// runForget is memory.forget: the tombstone's instant is taken inside
// execution; kblife.NewTombstone validates reason and principal.
func runForget(ctx context.Context, h dbstore.Handle, in opInput) (dblifecycle.Outcome, error) {
	if in.ID == "" || in.IfRevision < 1 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: forget needs a record ID and --if-revision", dbstore.ErrInvalidInput)
	}
	r, rev, err := getOn(ctx, h, in.ID)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if rev != in.IfRevision {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: revision mismatch on record %q: expected_revision %d, actual_revision %d", dbstore.ErrConflict, in.ID, in.IfRevision, rev)
	}
	if r.Tombstone != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: record %s already forgotten at %s", dbstore.ErrInvalidInput, r.ID, r.Tombstone.At.Format(time.RFC3339))
	}
	at := clock().UTC().Truncate(time.Second)
	tomb, err := kblife.NewTombstone("memory:"+r.ID, in.Reason, in.Source.Tool, in.Source.Episode, at)
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	r.Tombstone, r.Updated = &tomb, at.Format(time.RFC3339)
	patch, err := json.Marshal(map[string]any{"tombstone": tomb, "updated": r.Updated})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	res, err := onHandle(ctx, h, dbdoc.OpPatch, dbdoc.Input{Collection: Collection, ID: r.ID, Patch: patch, IfRevision: &rev})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var w struct {
		Revision int64 `json:"revision"`
	}
	if err := decode(res, &w); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return outcome(w.Revision, map[string]any{"record": r, "state": kblife.StateForgotten}), nil
}

// newID mints a stable record ID. It is never reused or derived from content.
func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("%w: record id: %v", dbstore.ErrBackend, err)
	}
	return "mem-" + hex.EncodeToString(b), nil
}
