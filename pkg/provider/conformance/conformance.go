// Package conformance is the reusable provider conformance suite
// (S472.14): one func over states and transitions, idempotency and
// reconciliation, normalized errors, unsupported operations,
// stop-versus-destroy billing and data semantics, and A6 destroy
// confirmation. It runs against maintained packages, declarative
// manifests in the master's engine mode (exec disabled) and executable
// plugins, fixture-only by default: live lifecycle checks (which
// allocate) run only with an explicit flag plus a separate resource
// authorization. It never touches the network beyond httptest.
package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// CallCounter reports how many provider-side operations took effect.
// A negative value means unknown: the suite skips that assertion.
type CallCounter interface {
	CreateCount() int
	DeleteCount() int
	RequestCount() int
}

// ExistenceChecker reports whether a resource still exists provider-side.
// When the setup does not provide one, existence assertions fall back to
// the dispatcher's journal alone.
type ExistenceChecker interface {
	Exists(id string) bool
}

// Setup is one provider under test: an opened dispatcher plus the
// seeded fixtures the suite may use. Counter is nil when the provider
// cannot report counts (count assertions are then skipped). An empty
// TempID skips the temporary-authorization-honored check; the
// temporary-authorization-refused check always runs.
type Setup struct {
	Dispatcher *provider.Dispatcher
	Counter    CallCounter
	RunningID  string
	StoppedID  string
	TempID     string
	// InjectCreateLoss arms a lost-response fault on the next create
	// (side effect happens, the answer never arrives). Nil skips the
	// ambiguous-create reconciliation check.
	InjectCreateLoss func()
}

// Factory opens the provider under test. Fake is the fixture HTTP API;
// factories that do not speak HTTP (executable plugins, fakedo-backed
// adapters) ignore it.
type Factory func(t *testing.T, fake *FakeAPI) Setup

// SpawnCounter counts executable-plugin spawns for the untouched
// assertions; creates and deletes are unknown (-1) for a plugin.
func SpawnCounter(spawns *atomic.Int64) CallCounter {
	return spawnCounter{spawns: spawns}
}

type spawnCounter struct {
	spawns *atomic.Int64
}

func (s spawnCounter) CreateCount() int  { return -1 }
func (s spawnCounter) DeleteCount() int  { return -1 }
func (s spawnCounter) RequestCount() int { return int(s.spawns.Load()) }

// ---- fixture HTTP API ----

// machine is one fixture resource in the scaffold's endpoint shape.
type machine struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Region    string   `json:"region"`
	Size      string   `json:"size"`
	Tags      []string `json:"tags"`
	IPs       []string `json:"ips"`
	CreatedAt string   `json:"created_at"`
	Retained  []string `json:"retained,omitempty"`
}

// FakeAPI is an httptest server speaking the scaffold's declarative
// endpoint shape (GET /api/account, GET /api/regions, GET+POST
// /api/machines, GET+DELETE /api/machines/{id}, POST
// /api/machines/{id}/power). It is seeded with deterministic fixtures
// — a running, a stopped and a temporary resource — and records
// creates, deletes and requests. SetStopDeletes corrupts it on
// purpose: stop then answers with destroy semantics, which conformance
// must detect.
type FakeAPI struct {
	*httptest.Server
	URL                 string
	RunningID           string
	StoppedID           string
	TempID              string
	key                 string
	mu                  sync.Mutex
	machines            map[string]*machine
	next                int
	creates, deletes    int
	requests            int
	losses              int
	stopDeletes         bool
	failCreateAfterSide bool
}

// NewFakeAPI starts the fixture API keyed by key, seeded for owner, and
// closes it when the test ends.
func NewFakeAPI(t *testing.T, key, owner string) *FakeAPI {
	t.Helper()
	f := &FakeAPI{
		key:       key,
		machines:  map[string]*machine{},
		RunningID: "fx-running-1",
		StoppedID: "fx-stopped-1",
		TempID:    "fx-temp-1",
	}
	now := time.Now().UTC().Format(time.RFC3339)
	f.machines[f.RunningID] = &machine{
		ID: f.RunningID, Name: "fx-running", Status: "running",
		Region: "r1", Size: "s-1vcpu-1gb",
		Tags:      provider.LeaseTags(owner, "seed-running", nil),
		IPs:       []string{"192.0.2.1"},
		CreatedAt: now,
	}
	f.machines[f.StoppedID] = &machine{
		ID: f.StoppedID, Name: "fx-stopped", Status: "stopped",
		Region: "r1", Size: "s-1vcpu-1gb",
		Tags:      provider.LeaseTags(owner, "seed-stopped", nil),
		IPs:       []string{"192.0.2.2"},
		CreatedAt: now,
	}
	f.machines[f.TempID] = &machine{
		ID: f.TempID, Name: "fx-temp", Status: "running",
		Region: "r1", Size: "s-1vcpu-1gb",
		Tags: provider.LeaseTags(owner, "seed-temp", &provider.Authorization{
			Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour),
		}),
		IPs:       []string{"192.0.2.3"},
		CreatedAt: now,
	}
	f.next = 100
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	f.URL = f.Server.URL
	return f
}

// SetStopDeletes makes the next stops answer with destroy semantics
// (the deliberately wrong fixture the suite must detect).
func (f *FakeAPI) SetStopDeletes(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopDeletes = v
}

// FailNextCreateAfterSideEffect makes the next create take effect and
// then answer 500, so the suite can prove reconciliation finds it
// without a second allocation.
func (f *FakeAPI) FailNextCreateAfterSideEffect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCreateAfterSide = true
}

// CreateCount is the number of creates that took effect (-1 never;
// the fake always knows).
func (f *FakeAPI) CreateCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.creates }

// DeleteCount is the number of deletes that took effect.
func (f *FakeAPI) DeleteCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.deletes }

// RequestCount is the number of authenticated API requests received.
func (f *FakeAPI) RequestCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

// Losses is the number of injected lost-response faults that fired.
func (f *FakeAPI) Losses() int { f.mu.Lock(); defer f.mu.Unlock(); return f.losses }

// Exists reports whether id is still stored.
func (f *FakeAPI) Exists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.machines[id]
	return ok
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *FakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		writeFakeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad credential"})
		return
	}
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "account" && r.Method == http.MethodGet:
		writeFakeJSON(w, 200, map[string]any{"account": map[string]string{"id": "acct-fixture", "status": "active"}})
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "regions" && r.Method == http.MethodGet:
		writeFakeJSON(w, 200, map[string]any{
			"regions": []map[string]string{{"slug": "region-1", "name": "Region 1"}},
			"sizes":   []map[string]string{{"slug": "small", "name": "Small"}},
		})
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodGet:
		f.mu.Lock()
		out := make([]machine, 0, len(f.machines))
		for _, m := range f.machines {
			out = append(out, *m)
		}
		f.mu.Unlock()
		writeFakeJSON(w, 200, map[string]any{"machines": out})
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodPost:
		var in struct {
			Name, Region, Size, Image string
			Tags                      []string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFakeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		f.mu.Lock()
		f.next++
		m := &machine{
			ID: fmt.Sprintf("mach-%d", f.next), Status: "running",
			Region: in.Region, Size: in.Size, Tags: in.Tags,
			IPs:       []string{"192.0.2.9"},
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		m.Name = in.Name
		if m.Name == "" {
			m.Name = m.ID
		}
		f.machines[m.ID] = m
		f.creates++
		fail := f.failCreateAfterSide
		f.failCreateAfterSide = false
		if fail {
			f.losses++
		}
		f.mu.Unlock()
		if fail {
			writeFakeJSON(w, 500, map[string]string{"error": "injected: side effect happened, answer lost"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"machine": m})
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodGet:
		f.mu.Lock()
		m, ok := f.machines[parts[2]]
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"machine": m})
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodDelete:
		f.mu.Lock()
		m, ok := f.machines[parts[2]]
		if ok {
			delete(f.machines, parts[2])
			f.deletes++
		}
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"machine": m})
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "machines" && parts[3] == "power" && r.Method == http.MethodPost:
		var in struct{ Action string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		m, ok := f.machines[parts[2]]
		if ok {
			switch in.Action {
			case "stop":
				if f.stopDeletes {
					m.Status = "deleted"
				} else {
					m.Status = "stopped"
				}
			case "start":
				m.Status = "running"
			}
		}
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"machine": m})
	default:
		writeFakeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// ---- suite ----

// Option configures a suite run.
type Option func(*config)

type config struct {
	liveAuth string
}

// WithLive enables the allocating lifecycle checks (create, ambiguous
// create, destroy) with auth as the resource authorization naming the
// run. Without it the suite is fixture-only and must allocate nothing.
func WithLive(auth string) Option {
	return func(c *config) { c.liveAuth = auth }
}

// Run executes the full suite against the factory's provider, failing
// the test on the first failing check.
func Run(t *testing.T, f Factory, fake *FakeAPI, opts ...Option) {
	t.Helper()
	if err := Check(t, f, fake, opts...); err != nil {
		t.Fatal(err)
	}
}

// Check executes the full suite and returns the first failing check's
// error, so a deliberately wrong fixture's detection can be asserted.
func Check(t *testing.T, f Factory, fake *FakeAPI, opts ...Option) error {
	t.Helper()
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	s := f(t, fake)
	if s.Dispatcher == nil {
		return fmt.Errorf("conformance: factory returned no dispatcher")
	}
	c := &checker{T: t, d: s.Dispatcher, counter: s.Counter, liveAuth: cfg.liveAuth, running: s}
	for _, step := range []struct {
		name string
		fn   func(*checker) error
	}{
		{"validate", (*checker).checkValidate},
		{"capacity", (*checker).checkCapacity},
		{"plan", (*checker).checkPlan},
		{"inspect-running", (*checker).checkInspect},
		{"list", (*checker).checkList},
		{"stop-start", (*checker).checkStopStart},
		{"unsupported-ops", (*checker).checkUnsupported},
		{"normalized-errors", (*checker).checkErrors},
		{"a6-refused", (*checker).checkDestroyRefused},
		{"a6-temporary", (*checker).checkTemporary},
		{"a6-confirmed-destroy", (*checker).checkConfirmedDestroy},
	} {
		if err := step.fn(c); err != nil {
			return fmt.Errorf("conformance %s: %w", step.name, err)
		}
	}
	if cfg.liveAuth == "" {
		return nil
	}
	for _, step := range []struct {
		name string
		fn   func(*checker) error
	}{
		{"live-create", (*checker).checkLiveCreate},
		{"live-ambiguous-create", (*checker).checkLiveAmbiguous},
		{"live-stop-start-destroy", (*checker).checkLiveTeardown},
	} {
		if err := step.fn(c); err != nil {
			return fmt.Errorf("conformance %s: %w", step.name, err)
		}
	}
	return nil
}

type checker struct {
	T        *testing.T
	d        *provider.Dispatcher
	counter  CallCounter
	liveAuth string
	running  Setup
	liveID   string
}

func (c *checker) do(req provider.Request) (provider.Result, error) {
	c.T.Helper()
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = provider.ProtocolVersion
	}
	return c.d.Do(c.T.Context(), req)
}

func codeOf(err error) provider.ErrorCode {
	var e *provider.Error
	if errorAs(err, &e) {
		return e.Code
	}
	return ""
}

func errorAs(err error, target **provider.Error) bool {
	for err != nil {
		if e, ok := err.(*provider.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// counts snapshots the known counters; a negative value stays negative
// (unknown) so assertions on it are skipped.
func (c *checker) counts() (creates, deletes, calls int) {
	creates, deletes, calls = -1, -1, -1
	if c.counter == nil {
		return creates, deletes, calls
	}
	return c.counter.CreateCount(), c.counter.DeleteCount(), c.counter.RequestCount()
}

func (c *checker) checkValidate() error {
	res, err := c.do(provider.Request{Op: provider.OpValidate})
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if len(res.Data) == 0 {
		return fmt.Errorf("validate: empty account payload")
	}
	return nil
}

func (c *checker) checkCapacity() error {
	if _, err := c.do(provider.Request{Op: provider.OpCapacity}); err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	return nil
}

func (c *checker) checkPlan() error {
	before, _, _ := c.counts()
	res, err := c.do(provider.Request{
		Op: provider.OpPlan, IdempotencyKey: "conf-plan-1",
		Spec: &provider.Spec{Name: "would-be", Region: "r1", Size: "s-1vcpu-1gb", Image: "img"},
	})
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if len(res.Data) == 0 {
		return fmt.Errorf("plan: empty plan payload")
	}
	if after, _, _ := c.counts(); before >= 0 && after != before {
		return fmt.Errorf("plan allocated: creates %d -> %d", before, after)
	}
	return nil
}

func (c *checker) checkInspect() error {
	id := c.running.RunningID
	res, err := c.do(provider.Request{Op: provider.OpInspect, ResourceID: id})
	if err != nil {
		return fmt.Errorf("inspect %s: %w", id, err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		return fmt.Errorf("inspect %s: state = %v, want running", id, res.Resource)
	}
	return nil
}

func (c *checker) checkList() error {
	res, err := c.do(provider.Request{Op: provider.OpList})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range res.Resources {
		seen[r.ID] = true
	}
	for _, id := range []string{c.running.RunningID, c.running.StoppedID} {
		if id != "" && !seen[id] {
			return fmt.Errorf("list: missing seeded fixture %s", id)
		}
	}
	if id := c.running.TempID; id != "" && !seen[id] {
		return fmt.Errorf("list: missing temporary fixture %s", id)
	}
	return nil
}

// checkStopStart proves stop is not destroy: the resource stays, keeps
// its identity and data, and keeps billing until destroyed.
func (c *checker) checkStopStart() error {
	id := c.running.RunningID
	before, err := c.do(provider.Request{Op: provider.OpInspect, ResourceID: id})
	if err != nil {
		return fmt.Errorf("pre-stop inspect %s: %w", id, err)
	}
	if before.Resource == nil {
		return fmt.Errorf("pre-stop inspect %s: no resource", id)
	}
	res, err := c.do(provider.Request{Op: provider.OpStop, IdempotencyKey: "conf-stop-1", ResourceID: id})
	if err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	if res.Resource == nil {
		return fmt.Errorf("stop %s: no resource in result", id)
	}
	if res.Resource.State != provider.StateStopped {
		return fmt.Errorf("stop %s: state = %q, want stopped (a stop that deletes is a destroy, not a stop)", id, res.Resource.State)
	}
	if res.Resource.Name != before.Resource.Name {
		return fmt.Errorf("stop %s: name changed %q -> %q: stop must retain data", id, before.Resource.Name, res.Resource.Name)
	}
	if res.Cost != nil && !res.Cost.Ongoing {
		return fmt.Errorf("stop %s: cost.ongoing = false: a stopped resource keeps billing", id)
	}
	if ex, ok := c.counter.(ExistenceChecker); ok && !ex.Exists(id) {
		return fmt.Errorf("stop %s: resource is gone provider-side: a stop must not delete", id)
	}
	res, err = c.do(provider.Request{Op: provider.OpStart, IdempotencyKey: "conf-start-1", ResourceID: id})
	if err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		return fmt.Errorf("start %s: state = %v, want running", id, res.Resource)
	}
	return nil
}

// checkUnsupported proves an operation outside the manifest never
// reaches the provider.
func (c *checker) checkUnsupported() error {
	createsBefore, deletesBefore, callsBefore := c.counts()
	m := c.d.Manifest()
	try := func(op provider.Op) error {
		_, err := c.do(provider.Request{Op: op, IdempotencyKey: "conf-un-" + string(op), ResourceID: c.running.RunningID})
		if codeOf(err) != provider.ErrUnsupportedCapability {
			return fmt.Errorf("%s: err = %v, want %s", op, err, provider.ErrUnsupportedCapability)
		}
		if after, afterDel, afterCalls := c.counts(); callsBefore >= 0 && (after != createsBefore || afterDel != deletesBefore || afterCalls != callsBefore) {
			return fmt.Errorf("%s reached the provider: counts changed", op)
		}
		return nil
	}
	if err := try(provider.Op("hibernate")); err != nil {
		return err
	}
	if !m.Supports(provider.OpReboot) {
		if err := try(provider.OpReboot); err != nil {
			return err
		}
	}
	return nil
}

// checkErrors proves failures are normalized, structured errors.
func (c *checker) checkErrors() error {
	// A well-formed but absent ID: every form must answer not_found
	// (a malformed ID is invalid_request, which is also normalized but
	// provider-specific).
	if _, err := c.do(provider.Request{Op: provider.OpInspect, ResourceID: "999999"}); codeOf(err) != provider.ErrNotFound {
		return fmt.Errorf("inspect missing: err = %v, want %s", err, provider.ErrNotFound)
	}
	createsBefore, deletesBefore, callsBefore := c.counts()
	if _, err := c.do(provider.Request{Op: provider.OpCreate, Spec: &provider.Spec{Name: "nokey"}}); codeOf(err) != provider.ErrInvalidRequest {
		return fmt.Errorf("create without idempotency key: err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	if after, afterDel, afterCalls := c.counts(); callsBefore >= 0 && (after != createsBefore || afterDel != deletesBefore || afterCalls != callsBefore) {
		return fmt.Errorf("invalid create reached the provider")
	}
	req := provider.Request{Op: provider.OpValidate, ProtocolVersion: "provider/v0"}
	if _, err := c.do(req); codeOf(err) != provider.ErrProtocolMismatch {
		return fmt.Errorf("wrong protocol: err = %v, want %s", err, provider.ErrProtocolMismatch)
	}
	return nil
}

// checkDestroyRefused is A6's core: a persistent destroy without a
// per-operation confirmation is refused before any provider call, the
// resource stays active, and the refusal reports ongoing cost.
func (c *checker) checkDestroyRefused() error {
	id := c.running.RunningID
	createsBefore, deletesBefore, callsBefore := c.counts()
	res, err := c.do(provider.Request{Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-naked", ResourceID: id})
	if codeOf(err) != provider.ErrConfirmationRequired {
		return fmt.Errorf("unconfirmed destroy of %s: err = %v, want %s", id, err, provider.ErrConfirmationRequired)
	}
	if after, afterDel, afterCalls := c.counts(); callsBefore >= 0 && (after != createsBefore || afterDel != deletesBefore || afterCalls != callsBefore) {
		return fmt.Errorf("refused destroy reached the provider")
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		return fmt.Errorf("refused destroy of %s must report ongoing cost: %+v", id, res.Cost)
	}
	if ex, ok := c.counter.(ExistenceChecker); ok && !ex.Exists(id) {
		return fmt.Errorf("refused destroy removed %s provider-side", id)
	}
	view, err := c.d.View(c.T.Context())
	if err != nil {
		return fmt.Errorf("journal view: %w", err)
	}
	if jr, ok := view.Resource(id); !ok || jr.Destroyed {
		return fmt.Errorf("journal after refused destroy: %+v", jr)
	}
	return nil
}

func tempAuth() *provider.Authorization {
	return &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour)}
}

// checkTemporary proves a temporary-lifecycle authorization is refused
// on a persistent resource and honored on a temporary one.
func (c *checker) checkTemporary() error {
	id := c.running.RunningID
	_, deletesBefore, callsBefore := c.counts()
	_, err := c.do(provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-temp-wrong",
		ResourceID: id, Authorization: tempAuth(),
	})
	if codeOf(err) != provider.ErrConfirmationRequired {
		return fmt.Errorf("temporary destroy of persistent %s: err = %v, want %s", id, err, provider.ErrConfirmationRequired)
	}
	if _, afterDel, afterCalls := c.counts(); callsBefore >= 0 && (afterDel != deletesBefore || afterCalls != callsBefore) {
		return fmt.Errorf("refused temporary destroy reached the provider")
	}
	if c.running.TempID == "" {
		return nil
	}
	tid := c.running.TempID
	res, err := c.do(provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-temp-ok",
		ResourceID: tid, Authorization: tempAuth(),
	})
	if err != nil {
		return fmt.Errorf("temporary destroy of %s: %w (a recorded TTL authorization must be honored)", tid, err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateDeleted {
		return fmt.Errorf("temporary destroy of %s: state = %v, want deleted", tid, res.Resource)
	}
	if ex, ok := c.counter.(ExistenceChecker); ok && ex.Exists(tid) {
		return fmt.Errorf("temporary destroy of %s left it provider-side", tid)
	}
	return nil
}

// checkConfirmedDestroy proves a confirmed persistent destroy deletes,
// and that the confirmation is single use by construction: replaying
// the same key answers from the journal without a second delete.
func (c *checker) checkConfirmedDestroy() error {
	id := c.running.StoppedID
	_, deletesBefore, _ := c.counts()
	res, err := c.do(provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-1",
		ResourceID: id, Authorization: confirm(id, "conf-destroy-1"),
	})
	if err != nil {
		return fmt.Errorf("confirmed destroy of %s: %w", id, err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateDeleted {
		return fmt.Errorf("confirmed destroy of %s: state = %v, want deleted", id, res.Resource)
	}
	if ex, ok := c.counter.(ExistenceChecker); ok && ex.Exists(id) {
		return fmt.Errorf("confirmed destroy of %s left it provider-side", id)
	}
	res, err = c.do(provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: "conf-destroy-1",
		ResourceID: id, Authorization: confirm(id, "conf-destroy-1"),
	})
	if err != nil {
		return fmt.Errorf("destroy replay of %s: %w (same key is the same operation)", id, err)
	}
	if res.Resource == nil || res.Resource.ID != id {
		return fmt.Errorf("destroy replay of %s answered a different resource: %+v", id, res.Resource)
	}
	if _, afterDel, _ := c.counts(); deletesBefore >= 0 && afterDel != deletesBefore+1 {
		return fmt.Errorf("destroy replay deleted again: deletes %d -> %d", deletesBefore, afterDel)
	}
	return nil
}

func confirm(id, key string) *provider.Authorization {
	return &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: key}
}

func spec(name string) *provider.Spec {
	// Values every maintained adapter accepts (the reference adapter
	// validates region and size against the API catalog).
	return &provider.Spec{Name: name, Region: "r1", Size: "s-1vcpu-1gb", Image: "img"}
}

// checkLiveCreate allocates with a fresh key and proves the idempotent
// replay finds the same resource without a second allocation.
func (c *checker) checkLiveCreate() error {
	createsBefore, _, _ := c.counts()
	key := "live-" + c.liveAuth + "-1"
	res, err := c.do(provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("live-one")})
	if err != nil {
		return fmt.Errorf("live create: %w", err)
	}
	if res.Resource == nil || res.Resource.ID == "" {
		return fmt.Errorf("live create: no resource: %+v", res)
	}
	c.liveID = res.Resource.ID
	if err := c.pollRunning(c.liveID); err != nil {
		return err
	}
	res, err = c.do(provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("live-one")})
	if err != nil {
		return fmt.Errorf("live create replay: %w", err)
	}
	if res.Resource == nil || res.Resource.ID != c.liveID {
		return fmt.Errorf("live create replay: ID changed to %v", res.Resource)
	}
	if after, _, _ := c.counts(); createsBefore >= 0 && after != createsBefore+1 {
		return fmt.Errorf("live create allocated %d, want exactly 1", after-createsBefore)
	}
	return nil
}

func (c *checker) pollRunning(id string) error {
	for range 20 {
		res, err := c.do(provider.Request{Op: provider.OpInspect, ResourceID: id})
		if err != nil {
			return fmt.Errorf("live inspect %s: %w", id, err)
		}
		if res.Resource != nil && res.Resource.State == provider.StateRunning {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("live resource %s never reached running", id)
}

// checkLiveAmbiguous loses the create answer after the side effect and
// proves reconciliation finds the resource with no second allocation.
func (c *checker) checkLiveAmbiguous() error {
	if c.running.InjectCreateLoss == nil {
		return nil
	}
	createsBefore, _, _ := c.counts()
	key := "live-" + c.liveAuth + "-ambiguous"
	c.running.InjectCreateLoss()
	res, err := c.do(provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("live-ambiguous")})
	if err != nil {
		return fmt.Errorf("ambiguous create: %w (reconciliation must recover it)", err)
	}
	if res.Resource == nil || res.Resource.ID == "" {
		return fmt.Errorf("ambiguous create: no resource: %+v", res)
	}
	if after, _, _ := c.counts(); createsBefore >= 0 && after != createsBefore+1 {
		return fmt.Errorf("ambiguous create allocated %d, want exactly 1", after-createsBefore)
	}
	return nil
}

// checkLiveTeardown stops, restarts and destroys the live resource with
// a per-operation confirmation, proving it is gone afterwards.
func (c *checker) checkLiveTeardown() error {
	id := c.liveID
	if _, err := c.do(provider.Request{Op: provider.OpStop, IdempotencyKey: "live-" + c.liveAuth + "-stop", ResourceID: id}); err != nil {
		return fmt.Errorf("live stop %s: %w", id, err)
	}
	if _, err := c.do(provider.Request{Op: provider.OpStart, IdempotencyKey: "live-" + c.liveAuth + "-start", ResourceID: id}); err != nil {
		return fmt.Errorf("live start %s: %w", id, err)
	}
	key := "live-" + c.liveAuth + "-destroy"
	res, err := c.do(provider.Request{
		Op: provider.OpDestroy, IdempotencyKey: key,
		ResourceID: id, Authorization: confirm(id, key),
	})
	if err != nil {
		return fmt.Errorf("live destroy %s: %w", id, err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateDeleted {
		return fmt.Errorf("live destroy %s: state = %v, want deleted", id, res.Resource)
	}
	if ex, ok := c.counter.(ExistenceChecker); ok && ex.Exists(id) {
		return fmt.Errorf("live destroy of %s left it provider-side", id)
	}
	return nil
}
