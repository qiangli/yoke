package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// The host/VM lifecycle dispatcher (S472.13a item 1). One Dispatcher
// fronts one provider — a built-in Go adapter, a declarative manifest run
// by the built-in engine, or an executable plugin — and gives every form
// the same create / inspect / list / start / stop / destroy path: the
// contract checks (Guard), the durable journal, common timeouts and
// retries, reconcile-by-idempotency-tag before any retry, and the A6
// destroy authorization. There is no built-in-only path.
//
// The master links this dispatcher with ExecDisabled: an executable plugin
// is then refused with ErrExecDisabled at Open, before any process can
// start. Local bashy and an existing satellite open it with ExecAllowed.
//
// Credentials are resolved once at Open through the caller's
// CredentialSource (pkg/secrets locally, a use-once grant or the vault
// token on the master — wired by the caller, never imported here), held in
// memory, handed to the declarative engine in memory, and scrubbed from
// every journal record and log line (the credential literal always; the
// caller's Redactor, pkg/redact wired from outside, on top).
//
// Host resource lifecycle is all this package knows: a running resource
// says nothing about whether a satellite is installed or ready (item 4).

// ExecPolicy says whether executable plugins may run in this process.
type ExecPolicy int

const (
	// ExecDisabled refuses every executable plugin (the master).
	ExecDisabled ExecPolicy = iota
	// ExecAllowed runs executable plugins (user-owned compute only).
	ExecAllowed
)

// BuiltinFactory builds a built-in adapter for a manifest from the
// credential the dispatcher resolved. Hosts register one per built-in
// provider name (e.g. digitalocean.New behind "digitalocean").
type BuiltinFactory func(ctx context.Context, m Manifest, credential string) (Adapter, error)

// CredentialSource resolves the provider credential. It is the one seam
// between this stdlib-only package and the host's credential store.
type CredentialSource interface {
	Credential(ctx context.Context, provider string) (string, error)
}

// StaticCredential is a CredentialSource holding one value in memory (a
// use-once token the caller already resolved).
type StaticCredential string

// Credential implements CredentialSource.
func (s StaticCredential) Credential(context.Context, string) (string, error) {
	return string(s), nil
}

// CredentialFunc adapts a function to CredentialSource.
type CredentialFunc func(ctx context.Context, provider string) (string, error)

// Credential implements CredentialSource.
func (f CredentialFunc) Credential(ctx context.Context, provider string) (string, error) {
	return f(ctx, provider)
}

// Redactor scrubs a string before it is journaled or logged. The caller
// wires pkg/redact's Scrubber.String here; the dispatcher always scrubs
// the credential literal itself, with or without a Redactor.
type Redactor func(string) string

// Options configures a Dispatcher.
type Options struct {
	// Owner scopes ownership tags (ValidOwner). Required.
	Owner string
	// Exec is the executable-plugin policy; the zero value is disabled.
	Exec ExecPolicy
	// Journal is the durable store; nil opens the file store under
	// DefaultJournalDir (BASHY_HOME/provider/journal).
	Journal JournalStore
	// Timeout bounds each provider call (default 60s).
	Timeout time.Duration
	// MutatingTimeout bounds a mutating call (create, destroy, power,
	// resize) when it is larger than Timeout; zero means Timeout.
	//
	// It exists because those two budgets are not the same size. A read
	// answers in a second or it is broken, but an adapter that brings a
	// host all the way to its serving shape before answering — AWS
	// Lightsail waits for "running" and then applies the firewall and
	// the static IP — legitimately runs for minutes. Bounding it at the
	// read timeout cancels the adapter mid-sequence and the dispatcher
	// then reconciles the create as "the resource exists", reporting
	// success for a host whose remaining steps never ran.
	MutatingTimeout time.Duration
	// Retries is how many more attempts a retryable failure gets after
	// the first (default 0: one attempt). A create is never retried
	// before its outcome has been reconciled by idempotency tag.
	Retries int
	// Backoff is the wait before a retry, multiplied by the attempt
	// number (default 0).
	Backoff time.Duration
	// Now is the clock (TTL expiry); nil means time.Now.
	Now func() time.Time
	// Redact is applied to every journaled and logged string after the
	// credential literal has been scrubbed.
	Redact Redactor
	// Credentials resolves the credential at Open. Required for the
	// built-in and declarative forms.
	Credentials CredentialSource
	// Builtins maps a built-in manifest name to its factory.
	Builtins map[string]BuiltinFactory
	// HTTPClient is used by the declarative engine (default 60s client).
	HTTPClient *http.Client
	// ExecStderr, ExecEnv and ExecDir configure the executable runner.
	// The dispatcher never puts the credential in ExecEnv.
	ExecStderr io.Writer
	ExecEnv    []string
	ExecDir    string
	// ExecFactory builds the executable runner (default NewExecAdapter).
	// Tests install a hook that fails when called.
	ExecFactory func(m Manifest, opts ExecOptions) (Adapter, error)
	// Log receives one redacted line per noteworthy event; nil is silent.
	Log func(line string)
}

// Dispatcher runs lifecycle operations for one provider through its
// adapter and the journal.
type Dispatcher struct {
	m       Manifest
	opts    Options
	adapter Adapter
	journal JournalStore
	secrets []string
	now     func() time.Time
}

// Open resolves the credential, builds the adapter for m's form and
// returns the dispatcher. It makes no provider call. An executable
// manifest under ExecDisabled is refused with ErrExecDisabled before any
// process starts.
func Open(ctx context.Context, m Manifest, opts Options) (*Dispatcher, error) {
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	if !ValidOwner(opts.Owner) {
		return nil, &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("dispatcher: owner %q must match [A-Za-z0-9_-]{1,64}", opts.Owner)}
	}
	if m.Form == FormExecutable && opts.Exec != ExecAllowed {
		return nil, &Error{Code: ErrExecDisabled, Message: fmt.Sprintf("provider %s is an executable plugin and this host does not run user-supplied code; run it on local bashy or an existing satellite", m.Name)}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.Backoff < 0 {
		opts.Backoff = 0
	}
	d := &Dispatcher{m: m, opts: opts, journal: opts.Journal, now: opts.Now}
	if d.now == nil {
		d.now = time.Now
	}
	if d.journal == nil {
		dir, err := DefaultJournalDir()
		if err != nil {
			return nil, err
		}
		fj, err := NewFileJournal(dir)
		if err != nil {
			return nil, err
		}
		d.journal = fj
	}
	var credential string
	if m.Form != FormExecutable {
		if opts.Credentials == nil {
			return nil, &Error{Code: ErrInvalidCredentials, Message: "provider " + m.Name + ": no credential source"}
		}
		c, err := opts.Credentials.Credential(ctx, m.Name)
		if err != nil {
			return nil, &Error{Code: ErrInvalidCredentials, Message: "provider " + m.Name + ": " + err.Error()}
		}
		credential = c
		d.addSecret(credential)
	}
	switch m.Form {
	case FormBuiltin:
		f := opts.Builtins[m.Name]
		if f == nil {
			return nil, &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("provider %s: no built-in adapter registered", m.Name)}
		}
		a, err := f(ctx, m, credential)
		if err != nil {
			return nil, err
		}
		d.adapter = Guard(a)
	case FormDeclarative:
		a, err := NewDeclarativeAdapter(m, DeclarativeConfig{Credential: credential, Owner: opts.Owner, HTTPClient: opts.HTTPClient})
		if err != nil {
			return nil, err
		}
		d.adapter = a
	case FormExecutable:
		// The frozen executable protocol carries the Request alone on
		// stdin; the credential rides stdin once S472.4a adds it to the
		// document. It is never placed in the environment.
		env := opts.ExecEnv
		for _, s := range d.secrets {
			for _, kv := range env {
				if s != "" && strings.Contains(kv, s) {
					return nil, &Error{Code: ErrInvalidRequest, Message: "provider " + m.Name + ": the credential may not be passed in the environment"}
				}
			}
		}
		f := opts.ExecFactory
		if f == nil {
			f = NewExecAdapter
		}
		a, err := f(m, ExecOptions{Stderr: opts.ExecStderr, Env: env, Dir: opts.ExecDir})
		if err != nil {
			return nil, err
		}
		d.adapter = Guard(a)
	}
	return d, nil
}

// Manifest returns the provider's manifest.
func (d *Dispatcher) Manifest() Manifest { return d.m }

// Adapter returns the underlying adapter (guarded). Callers that need a
// raw read (inspect/list without the dispatcher) may use it.
func (d *Dispatcher) Adapter() Adapter { return d.adapter }

func (d *Dispatcher) addSecret(s string) {
	if s == "" {
		return
	}
	for _, v := range []string{s, url.QueryEscape(s)} {
		if !slices.Contains(d.secrets, v) {
			d.secrets = append(d.secrets, v)
		}
	}
}

// Redact scrubs s: the credential literal(s), then the caller's Redactor.
func (d *Dispatcher) Redact(s string) string {
	for _, sec := range d.secrets {
		s = strings.ReplaceAll(s, sec, "[REDACTED:credential]")
	}
	if d.opts.Redact != nil {
		s = d.opts.Redact(s)
	}
	return s
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.opts.Log != nil {
		d.opts.Log(d.Redact(fmt.Sprintf(format, args...)))
	}
}

// ---- journal helpers ----

func (d *Dispatcher) entry(req Request, phase Phase, attempt int) JournalEntry {
	e := JournalEntry{
		At: d.now().UTC(), Provider: d.m.Name, Form: d.m.Form, Op: req.Op,
		IdempotencyKey: req.IdempotencyKey, Phase: phase, Attempt: attempt,
		ResourceID: req.ResourceID, Authorization: req.Authorization,
	}
	if req.Spec != nil {
		s := *req.Spec
		s.UserData = nil // opaque and possibly secret-bearing: never journaled
		if len(req.Spec.UserData) > 0 {
			e.Note = "user_data omitted"
		}
		e.Spec = &s
	}
	if req.Op == OpCreate {
		e.Tags = LeaseTags(d.opts.Owner, req.IdempotencyKey, req.Authorization)
	}
	return e
}

func (d *Dispatcher) record(ctx context.Context, e JournalEntry) error {
	e = d.redactEntry(e)
	if err := d.journal.Append(ctx, e); err != nil {
		return &Error{Code: ErrProvider, Message: "journal: " + err.Error()}
	}
	return nil
}

func (d *Dispatcher) redactEntry(e JournalEntry) JournalEntry {
	r := d.Redact
	e.IdempotencyKey, e.ResourceID, e.ResultID, e.Note = r(e.IdempotencyKey), r(e.ResourceID), r(e.ResultID), r(e.Note)
	e.Tags = d.redactStrings(e.Tags)
	if e.Spec != nil {
		s := *e.Spec
		s.Name, s.Region, s.Size, s.Image = r(s.Name), r(s.Region), r(s.Size), r(s.Image)
		s.Tags = d.redactStrings(s.Tags)
		if s.Labels != nil {
			l := make(map[string]string, len(s.Labels))
			for k, v := range s.Labels {
				l[r(k)] = r(v)
			}
			s.Labels = l
		}
		s.UserData = nil
		e.Spec = &s
	}
	if e.Authorization != nil {
		a := *e.Authorization
		a.ResourceID, a.IdempotencyKey, a.WorkRef = r(a.ResourceID), r(a.IdempotencyKey), r(a.WorkRef)
		e.Authorization = &a
	}
	if e.Error != nil {
		er := *e.Error
		er.Message = r(er.Message)
		e.Error = &er
	}
	if e.Cost != nil {
		c := *e.Cost
		c.Note = r(c.Note)
		c.Retained = d.redactStrings(c.Retained)
		e.Cost = &c
	}
	return e
}

func (d *Dispatcher) redactStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = d.Redact(s)
	}
	return out
}

// View loads and folds the provider's journal.
func (d *Dispatcher) View(ctx context.Context) (JournalView, error) {
	es, err := d.journal.Load(ctx, d.m.Name)
	if err != nil {
		return JournalView{}, &Error{Code: ErrProvider, Message: "journal: " + err.Error()}
	}
	return Replay(es), nil
}

// ---- Do ----

// Do runs one operation. Read operations (validate, capacity, plan,
// inspect, list) go to the adapter under the common timeout and retries
// and are not journaled. A mutating operation is journaled before and
// after the call; a replay of a completed key answers from the journal
// without a provider call — except a create, whose resource is proven to
// still exist first (createReplay); a pending key is reconciled first;
// and a destroy is authorized per A6 before anything reaches the adapter.
func (d *Dispatcher) Do(ctx context.Context, req Request) (Result, error) {
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = ProtocolVersion
	}
	if e := CheckRequest(d.m, req); e != nil {
		r := Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e}
		if e.Code == ErrConfirmationRequired {
			r.Cost = RefusedDestroyCost()
		}
		if req.Op.Mutating() && req.IdempotencyKey != "" {
			je := d.entry(req, PhaseRefused, 0)
			je.Error, je.Cost = e, r.Cost
			if jerr := d.record(ctx, je); jerr != nil {
				return Failure(req.Op, ErrProvider, "%v", jerr)
			}
		}
		return r, e
	}
	if !req.Op.Mutating() {
		res, err, _ := d.attempt(ctx, req)
		return res, err
	}
	view, err := d.View(ctx)
	if err != nil {
		return Failure(req.Op, ErrProvider, "%v", err)
	}
	if op, ok := view.Find(req.Op, req.IdempotencyKey); ok {
		if req.ResourceID != "" && op.ResourceID != "" && op.ResourceID != req.ResourceID {
			// One key is one operation on one resource; a confirmation
			// bound to that key cannot be carried to another resource.
			return Failure(req.Op, ErrInvalidRequest, "idempotency key %q was already used for %s of resource %s", req.IdempotencyKey, req.Op, op.ResourceID)
		}
		switch {
		case op.Done && !op.Retired:
			if req.Op == OpCreate {
				res, stale, rerr := d.createReplay(ctx, req, op, view)
				if rerr != nil {
					return Failure(req.Op, ErrProvider, "%v", rerr)
				}
				if !stale {
					d.logf("create %s: replay answered from the journal", req.IdempotencyKey)
					return res, nil
				}
				// The old answer has been retired: fall through and
				// create, exactly as a first call with this key would.
				break
			}
			// Idempotent replay: the same key is the same operation; a
			// destroy confirmation is thereby single use.
			d.logf("%s %s: replay answered from the journal", req.Op, req.IdempotencyKey)
			return d.resultFromEntry(req, op.Result), nil
		case op.NeedsManualReconcile:
			return Failure(req.Op, ErrNeedsManualReconcile, "%s %s: outcome unproven; reconcile manually before retrying", req.Op, req.IdempotencyKey)
		case op.Pending:
			res, settled, err := d.reconcile(ctx, req, op)
			if err != nil {
				return Failure(req.Op, ErrNeedsManualReconcile, "%v", err)
			}
			if settled {
				return res, res.Err()
			}
		}
	}
	if req.Op == OpDestroy {
		if r, e := d.authorizeDestroy(ctx, req, view); e != nil {
			return r, e
		}
		res, err := d.run(ctx, req)
		if err == nil {
			d.retireCreates(ctx, view, req.ResourceID)
		}
		return res, err
	}
	return d.run(ctx, req)
}

// authorizeDestroy is the dispatcher's half of A6 on top of the static
// check: a temporary authorization is honored only for a resource the
// journal recorded as temporary at create (an unknown resource falls
// through to the adapter's tag check), and a persistent resource known to
// the journal always needs the per-operation confirmation.
func (d *Dispatcher) authorizeDestroy(ctx context.Context, req Request, view JournalView) (Result, error) {
	a := req.Authorization
	jr, known := view.Resource(req.ResourceID)
	if !known || a == nil || a.Kind != AuthTemporary {
		return Result{}, nil
	}
	refuse := func(why string) (Result, error) {
		e := &Error{Code: ErrConfirmationRequired, Message: fmt.Sprintf("destroy of %s refused: %s; the resource is untouched and keeps its ongoing cost", req.ResourceID, why)}
		je := d.entry(req, PhaseRefused, 0)
		je.Error, je.Cost = e, RefusedDestroyCost()
		if jerr := d.record(ctx, je); jerr != nil {
			return Failure(req.Op, ErrProvider, "%v", jerr)
		}
		return Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e, Cost: je.Cost}, e
	}
	if jr.Temporary == nil {
		return refuse("the journal records it as persistent; a per-operation confirmation is required")
	}
	if a.WorkRef != "" && jr.Temporary.WorkRef != a.WorkRef {
		return refuse(fmt.Sprintf("temporary authorization names work %q, the resource was created for %q", a.WorkRef, jr.Temporary.WorkRef))
	}
	return Result{}, nil
}

// createReplay decides whether a completed create still answers its key.
//
// A create is the one operation whose journal entry the world can make
// obsolete: the resource it produced may have been destroyed since — here
// with another key, by a TTL sweep, or in the provider's own console —
// and replaying the old result would then report a running host to a
// caller who asked for one, without making a single provider call. So the
// replay is PROVEN before it is served: the resource is inspected, the
// fresh state is what the caller gets, and a resource that is gone
// retires the entry (PhaseRetired) so the key creates anew.
//
// An inspect that merely FAILS proves nothing and must not cause a second
// create: the journal's answer stands.
func (d *Dispatcher) createReplay(ctx context.Context, req Request, op Operation, view JournalView) (res Result, stale bool, err error) {
	journaled := d.resultFromEntry(req, op.Result)
	id := op.ResourceID
	if id == "" {
		// Nothing to prove it against (a create that reported no
		// resource); the journal is all there is.
		return journaled, false, nil
	}
	retire := func(why string) (Result, bool, error) {
		d.logf("create %s: %s is %s; retiring the journal entry so the key creates anew", req.IdempotencyKey, id, why)
		e := JournalEntry{
			At: d.now().UTC(), Provider: d.m.Name, Form: d.m.Form, Op: OpCreate,
			IdempotencyKey: op.IdempotencyKey, Phase: PhaseRetired, ResourceID: id,
			Note: "create replay retired: " + why,
		}
		if rerr := d.record(ctx, e); rerr != nil {
			return Result{}, false, rerr
		}
		return Result{}, true, nil
	}
	if jr, known := view.Resource(id); known && jr.Destroyed {
		// The journal itself says it is gone; no provider call needed.
		return retire("recorded destroyed in the journal")
	}
	if !d.m.Supports(OpInspect) {
		return journaled, false, nil
	}
	ir, ierr, _ := d.attempt(ctx, Request{ProtocolVersion: ProtocolVersion, Op: OpInspect, ResourceID: id})
	if ierr != nil {
		var e *Error
		if errors.As(ierr, &e) && e.Code == ErrNotFound {
			return retire("gone at the provider")
		}
		d.logf("create %s: inspect of %s failed (%v); answering the replay from the journal", req.IdempotencyKey, id, ierr)
		return journaled, false, nil
	}
	if ir.Resource == nil {
		return journaled, false, nil
	}
	if ir.Resource.State == StateDeleted {
		return retire("deleted at the provider")
	}
	// Alive: report what it is NOW, not what it was when it was created.
	return Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Resource: ir.Resource, Cost: journaled.Cost}, false, nil
}

// retireCreates marks every completed create of resourceID dead, so the
// next create with that key really creates instead of replaying a host
// that this destroy just removed. A failure to record is logged, not
// returned: the destroy itself succeeded, and createReplay's liveness
// check catches the same thing one provider call later.
func (d *Dispatcher) retireCreates(ctx context.Context, view JournalView, resourceID string) {
	if resourceID == "" {
		return
	}
	for _, op := range view.Operations {
		if op.Op != OpCreate || op.ResourceID != resourceID || !op.Done || op.Retired {
			continue
		}
		e := JournalEntry{
			At: d.now().UTC(), Provider: d.m.Name, Form: d.m.Form, Op: OpCreate,
			IdempotencyKey: op.IdempotencyKey, Phase: PhaseRetired, ResourceID: resourceID,
			Note: "resource destroyed",
		}
		if err := d.record(ctx, e); err != nil {
			d.logf("destroy %s: could not retire create %s: %v", resourceID, op.IdempotencyKey, err)
			return
		}
		d.logf("destroy %s: retired create %s", resourceID, op.IdempotencyKey)
	}
}

func (d *Dispatcher) resultFromEntry(req Request, e *JournalEntry) Result {
	r := Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Cost: e.Cost}
	id := e.ResultID
	if id == "" {
		id = e.ResourceID
	}
	if id != "" {
		r.Resource = &Resource{ID: id, State: e.State, Tags: e.Tags}
	}
	return r
}

// run is the attempt loop for a mutating op: intent → call → result, with
// reconciliation before every retry.
func (d *Dispatcher) run(ctx context.Context, req Request) (Result, error) {
	for attempt := 1; ; attempt++ {
		if err := d.record(ctx, d.entry(req, PhaseIntent, attempt)); err != nil {
			return Failure(req.Op, ErrProvider, "%v", err)
		}
		res, err, ambiguous := d.attempt(ctx, req)
		if !ambiguous {
			je := d.entry(req, PhaseResult, attempt)
			d.fill(&je, res)
			if err != nil && res.Error != nil && res.Error.Code == ErrConfirmationRequired {
				je.Phase = PhaseRefused
			}
			if err != nil && req.Op == OpDestroy && je.Cost == nil {
				je.Cost = RefusedDestroyCost()
				res.Cost = je.Cost
			}
			if jerr := d.record(ctx, je); jerr != nil {
				return Failure(req.Op, ErrProvider, "%v", jerr)
			}
			return res, err
		}
		je := d.entry(req, PhaseAmbiguous, attempt)
		d.fill(&je, res)
		if jerr := d.record(ctx, je); jerr != nil {
			return Failure(req.Op, ErrProvider, "%v", jerr)
		}
		d.logf("%s %s: attempt %d ambiguous (%v); reconciling before any retry", req.Op, req.IdempotencyKey, attempt, err)
		op := Operation{Op: req.Op, IdempotencyKey: req.IdempotencyKey, ResourceID: req.ResourceID, Attempts: attempt, Pending: true}
		settledRes, settled, rerr := d.reconcile(ctx, req, op)
		if rerr != nil {
			return Failure(req.Op, ErrNeedsManualReconcile, "%v", rerr)
		}
		if settled {
			return settledRes, settledRes.Err()
		}
		if attempt > d.opts.Retries || ctx.Err() != nil {
			// Reconciliation proved nothing happened; out of attempts.
			je := d.entry(req, PhaseResult, attempt)
			d.fill(&je, res)
			if jerr := d.record(ctx, je); jerr != nil {
				return Failure(req.Op, ErrProvider, "%v", jerr)
			}
			return res, err
		}
		if err := d.sleep(ctx, time.Duration(attempt)*d.opts.Backoff); err != nil {
			return Failure(req.Op, ErrTimeout, "%v", err)
		}
	}
}

func (d *Dispatcher) fill(je *JournalEntry, res Result) {
	je.Error, je.Cost = res.Error, res.Cost
	if r := res.Resource; r != nil {
		je.ResultID, je.State, je.Tags = r.ID, r.State, r.Tags
	}
}

// timeoutFor is the per-call budget for one operation: MutatingTimeout
// for a mutating op when it is the larger of the two, else Timeout.
func (d *Dispatcher) timeoutFor(op Op) time.Duration {
	if op.Mutating() && d.opts.MutatingTimeout > d.opts.Timeout {
		return d.opts.MutatingTimeout
	}
	return d.opts.Timeout
}

// attempt performs one guarded adapter call under the common timeout.
// ambiguous is true when the failure leaves open that the provider acted.
func (d *Dispatcher) attempt(ctx context.Context, req Request) (res Result, err error, ambiguous bool) {
	cctx, cancel := context.WithTimeout(ctx, d.timeoutFor(req.Op))
	defer cancel()
	res, err = d.adapter.Do(cctx, req)
	if err == nil {
		return res, nil, false
	}
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: ErrProvider, Message: err.Error()}
		res = Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e}
		err = e
	}
	switch e.Code {
	case ErrTimeout, ErrRateLimited:
		return res, err, true
	case ErrProvider:
		return res, err, e.Retryable
	}
	return res, err, false
}

func (d *Dispatcher) sleep(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- reconciliation ----

// reconcile asks the provider what became of a pending operation and
// journals the answer. settled is true when the operation is proven done
// (res is its result). A false settled with nil err means the operation
// provably did not happen and may be retried. A non-nil err means the
// outcome is unprovable: it has been journaled as needs_manual_reconcile.
func (d *Dispatcher) reconcile(ctx context.Context, req Request, op Operation) (res Result, settled bool, err error) {
	manual := func(why string) (Result, bool, error) {
		e := &Error{Code: ErrNeedsManualReconcile, Message: fmt.Sprintf("%s %s: %s", req.Op, req.IdempotencyKey, why)}
		je := d.entry(req, PhaseNeedsManualReconcile, op.Attempts)
		je.Error = e
		if jerr := d.record(ctx, je); jerr != nil {
			return Result{}, false, jerr
		}
		d.logf("%s %s: needs manual reconcile: %s", req.Op, req.IdempotencyKey, why)
		return Result{}, false, e
	}
	done := func(r *Resource, cost *CostReport, note string) (Result, bool, error) {
		je := d.entry(req, PhaseReconciled, op.Attempts)
		je.Note = note
		out := Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Resource: r, Cost: cost}
		d.fill(&je, out)
		if jerr := d.record(ctx, je); jerr != nil {
			return Result{}, false, jerr
		}
		d.logf("%s %s: reconciled from the provider: %s", req.Op, req.IdempotencyKey, note)
		return out, true, nil
	}
	notDone := func(note string) (Result, bool, error) {
		je := d.entry(req, PhaseReconciled, op.Attempts)
		je.Note = note
		je.Error = &Error{Code: ErrProvider, Message: "not applied: " + note}
		if jerr := d.record(ctx, je); jerr != nil {
			return Result{}, false, jerr
		}
		return Result{}, false, nil
	}
	rctx, cancel := context.WithTimeout(ctx, d.opts.Timeout)
	defer cancel()
	switch req.Op {
	case OpCreate:
		if !d.m.Supports(OpList) {
			return manual("provider cannot list; the create cannot be proven either way")
		}
		lr, err := d.adapter.Do(rctx, Request{ProtocolVersion: ProtocolVersion, Op: OpList})
		if err != nil {
			return manual("list by idempotency tag failed: " + err.Error())
		}
		tag := IdempotencyTag(req.IdempotencyKey)
		var mine []Resource
		for _, r := range lr.Resources {
			if Owned(r.Tags, d.opts.Owner) && slices.Contains(r.Tags, tag) {
				mine = append(mine, r)
			}
		}
		switch len(mine) {
		case 0:
			return notDone("no resource carries the idempotency tag")
		case 1:
			r := mine[0]
			return done(&r, nil, "found by idempotency tag")
		}
		ids := make([]string, len(mine))
		for i, r := range mine {
			ids[i] = r.ID
		}
		return manual(fmt.Sprintf("idempotency tag matches %d resources (%s)", len(mine), strings.Join(ids, ", ")))
	case OpDestroy, OpStart, OpStop, OpReboot, OpResize:
		if !d.m.Supports(OpInspect) {
			return manual("provider cannot inspect; the outcome cannot be proven")
		}
		ir, err := d.adapter.Do(rctx, Request{ProtocolVersion: ProtocolVersion, Op: OpInspect, ResourceID: req.ResourceID})
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == ErrNotFound {
				if req.Op == OpDestroy {
					return done(&Resource{ID: req.ResourceID, State: StateDeleted}, &CostReport{Note: "destroyed; retained attachments unknown after the fact"}, "resource is gone")
				}
				return manual("resource no longer exists")
			}
			return manual("inspect failed: " + err.Error())
		}
		r := ir.Resource
		switch req.Op {
		case OpDestroy:
			if r.State == StateDeleted || r.State == StateDeleting {
				return done(r, nil, "provider reports it deleted")
			}
			return notDone("resource still exists")
		case OpStart, OpReboot:
			if r.State == StateRunning {
				return done(r, nil, "resource is running")
			}
			return notDone("resource is " + string(r.State))
		case OpStop:
			if r.State == StateStopped {
				return done(r, &CostReport{Ongoing: true, Retained: []string{"resource:" + r.ID}, Note: "a stopped resource keeps billing until destroyed"}, "resource is stopped")
			}
			return notDone("resource is " + string(r.State))
		case OpResize:
			if req.Spec != nil && r.Size == req.Spec.Size {
				return done(r, nil, "resource has the requested size")
			}
			return notDone("resource is still " + r.Size)
		}
	}
	return manual("no reconciliation rule for " + string(req.Op))
}

// ResumeReport summarizes Resume.
type ResumeReport struct {
	Pending              int // operations found pending
	Reconciled           int // proven done, result journaled
	NotApplied           int // proven not done; a replay may retry
	NeedsManualReconcile int
}

// Resume reconciles every pending operation in the journal — a create
// whose result was never recorded, an ambiguous destroy — from the
// provider, before anything is retried. It never creates or destroys: it
// only asks and records. Call it once at process start.
func (d *Dispatcher) Resume(ctx context.Context) (ResumeReport, error) {
	view, err := d.View(ctx)
	if err != nil {
		return ResumeReport{}, err
	}
	var rep ResumeReport
	for _, op := range view.Pending() {
		rep.Pending++
		req := Request{ProtocolVersion: ProtocolVersion, Op: op.Op, IdempotencyKey: op.IdempotencyKey, ResourceID: op.ResourceID}
		if op.Op == OpCreate {
			req.ResourceID = ""
		}
		_, settled, rerr := d.reconcile(ctx, req, op)
		switch {
		case rerr != nil:
			rep.NeedsManualReconcile++
		case settled:
			rep.Reconciled++
		default:
			rep.NotApplied++
		}
	}
	return rep, nil
}

// ---- temporary lifecycle (A6) ----

// TemporaryKey is the idempotency key of the automatic destroy of a
// temporary resource created under createKey: deterministic, so expiry
// sweeps and work completion destroy it exactly once.
func TemporaryKey(createKey string) string { return "temporary-destroy:" + createKey }

// ExpireTemporary destroys every journaled resource whose TTL
// authorization has expired by the dispatcher's clock, with no
// confirmation input: the authorization was given at create. Each
// resource is destroyed exactly once (TemporaryKey). Failures are
// reported per resource and leave it journaled active with its cost.
func (d *Dispatcher) ExpireTemporary(ctx context.Context) ([]Result, error) {
	return d.sweep(ctx, func(r JournaledResource) bool {
		return !r.Temporary.ExpiresAt.IsZero() && !d.now().Before(r.Temporary.ExpiresAt)
	}, nil)
}

// CompleteWork destroys every journaled work-bound temporary resource
// created for workRef, with no confirmation input.
func (d *Dispatcher) CompleteWork(ctx context.Context, workRef string) ([]Result, error) {
	if workRef == "" {
		return nil, &Error{Code: ErrInvalidRequest, Message: "work ref is required"}
	}
	return d.sweep(ctx, func(r JournaledResource) bool { return r.Temporary.WorkRef == workRef }, &workRef)
}

func (d *Dispatcher) sweep(ctx context.Context, due func(JournaledResource) bool, workRef *string) ([]Result, error) {
	view, err := d.View(ctx)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, r := range view.Resources {
		if r.Temporary == nil || r.Destroyed || r.CreateKey == "" || !due(r) {
			continue
		}
		if op, ok := view.Find(OpDestroy, TemporaryKey(r.CreateKey)); ok && (op.Done || op.NeedsManualReconcile) {
			continue
		}
		auth := &Authorization{Kind: AuthTemporary}
		if workRef != nil {
			auth.WorkRef = *workRef
		}
		res, _ := d.Do(ctx, Request{Op: OpDestroy, IdempotencyKey: TemporaryKey(r.CreateKey), ResourceID: r.ID, Authorization: auth})
		out = append(out, res)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := "", ""
		if out[i].Resource != nil {
			ai = out[i].Resource.ID
		}
		if out[j].Resource != nil {
			aj = out[j].Resource.ID
		}
		return ai < aj
	})
	return out, nil
}
