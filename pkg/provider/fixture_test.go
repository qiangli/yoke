package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// The fixture cloud: one in-memory machine store with the faults the
// S472.13a scenario needs (async transitions, a stall after the side
// effect, a 5xx before it, retained attachments). It is served two ways:
// as the fixture HTTP provider API behind the declarative manifest, and as
// the state behind the executable fixture plugin, persisted to a file so
// it survives the plugin's one-request-per-process lifetime.

const (
	fixtureKey = "fixture-api-key-7f3a9c"
	stateEnv   = "PROVIDER_TEST_STATE"
	fixtureURL = "PROVIDER_TEST_URL"
)

type machine struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // new, running, stopped, deleted
	Region    string    `json:"region"`
	Size      string    `json:"size"`
	Image     string    `json:"image"`
	Tags      []string  `json:"tags"`
	IPs       []string  `json:"ips"`
	Retained  []string  `json:"retained"`
	CreatedAt time.Time `json:"created_at"`
	Polls     int       `json:"polls"`
}

type cloudState struct {
	Next       int                 `json:"next"`
	Machines   map[string]*machine `json:"machines"`
	AsyncPolls int                 `json:"async_polls"`
	StallNext  int                 `json:"stall_next"` // creates that stall after the side effect
	Fail5xx    int                 `json:"fail_5xx"`   // creates that 503 before the side effect
	Creates    int                 `json:"creates"`
	Deletes    int                 `json:"deletes"`
	Calls      int                 `json:"calls"` // provider calls (HTTP requests / plugin runs)
}

type fakeCloud struct {
	mu   sync.Mutex
	st   cloudState
	path string // when set, every mutation is persisted here
}

func newFakeCloud(path string) *fakeCloud {
	c := &fakeCloud{path: path, st: cloudState{Machines: map[string]*machine{}}}
	c.load()
	return c
}

func (c *fakeCloud) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var st cloudState
	if json.Unmarshal(data, &st) == nil {
		if st.Machines == nil {
			st.Machines = map[string]*machine{}
		}
		c.st = st
	}
}

func (c *fakeCloud) save() {
	if c.path == "" {
		return
	}
	data, _ := json.Marshal(c.st)
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, c.path)
	}
}

// with runs f under the lock, reloading persisted state first and saving
// after, so the HTTP fixture and the plugin see one store.
func (c *fakeCloud) with(f func(st *cloudState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	f(&c.st)
	c.save()
}

func (c *fakeCloud) setAsync(n int)     { c.with(func(st *cloudState) { st.AsyncPolls = n }) }
func (c *fakeCloud) stallNextCreate()   { c.with(func(st *cloudState) { st.StallNext++ }) }
func (c *fakeCloud) fail5xxNextCreate() { c.with(func(st *cloudState) { st.Fail5xx++ }) }
func (c *fakeCloud) counts() (creates, deletes, calls int) {
	c.with(func(st *cloudState) { creates, deletes, calls = st.Creates, st.Deletes, st.Calls })
	return
}
func (c *fakeCloud) attachRetained(id, what string) {
	c.with(func(st *cloudState) {
		if m := st.Machines[id]; m != nil {
			m.Retained = append(m.Retained, what)
		}
	})
}
func (c *fakeCloud) exists(id string) (ok bool) {
	c.with(func(st *cloudState) { _, ok = st.Machines[id] })
	return
}

// seed adds a machine the dispatcher did not create.
func (c *fakeCloud) seed(name string, tags ...string) (id string) {
	c.with(func(st *cloudState) {
		m := newMachine(st, name, "r1", "s-1", "img", tags)
		m.Status = "running"
		id = m.ID
	})
	return
}

func newMachine(st *cloudState, name, region, size, image string, tags []string) *machine {
	st.Next++
	id := strconv.Itoa(5000 + st.Next)
	m := &machine{
		ID: id, Name: name, Status: "new", Region: region, Size: size, Image: image,
		Tags: slices.Clone(tags), IPs: []string{fmt.Sprintf("192.0.2.%d", st.Next%250+1)},
		Retained:  []string{},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(st.Next) * time.Second),
		Polls:     st.AsyncPolls,
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	if m.Polls == 0 {
		m.Status = "running"
	}
	st.Machines[id] = m
	return m
}

func tick(m *machine) {
	if m.Status == "new" {
		if m.Polls--; m.Polls <= 0 {
			m.Status = "running"
		}
	}
}

// cloudErr is a provider-side failure with an HTTP-shaped status.
type cloudErr struct {
	status int
	msg    string
}

func (e *cloudErr) Error() string { return e.msg }

// create performs the side effect and reports whether the response must
// then be withheld (stall) — the fault the scenario recovers from.
func (c *fakeCloud) create(name, region, size, image string, tags []string) (m machine, stall bool, err error) {
	c.with(func(st *cloudState) {
		st.Calls++
		if st.Fail5xx > 0 {
			st.Fail5xx--
			err = &cloudErr{503, "injected 503 before the side effect"}
			return
		}
		if name == "" || region == "" || size == "" || image == "" {
			err = &cloudErr{422, "name, region, size and image are required"}
			return
		}
		mm := newMachine(st, name, region, size, image, tags)
		st.Creates++
		if st.StallNext > 0 {
			st.StallNext--
			stall = true
		}
		m = *mm
	})
	return
}

func (c *fakeCloud) list() (out []machine) {
	c.with(func(st *cloudState) {
		st.Calls++
		for _, m := range st.Machines {
			tick(m)
			out = append(out, *m)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []machine{}
	}
	return
}

func (c *fakeCloud) get(id string) (m machine, err error) {
	c.with(func(st *cloudState) {
		st.Calls++
		mm := st.Machines[id]
		if mm == nil {
			err = &cloudErr{404, "no such machine"}
			return
		}
		tick(mm)
		m = *mm
	})
	return
}

func (c *fakeCloud) power(id, action string) (m machine, err error) {
	c.with(func(st *cloudState) {
		st.Calls++
		mm := st.Machines[id]
		if mm == nil {
			err = &cloudErr{404, "no such machine"}
			return
		}
		switch action {
		case "start":
			mm.Status = "running"
		case "stop":
			mm.Status = "stopped"
		default:
			err = &cloudErr{422, "unknown action " + action}
			return
		}
		m = *mm
	})
	return
}

func (c *fakeCloud) remove(id string) (m machine, err error) {
	c.with(func(st *cloudState) {
		st.Calls++
		mm := st.Machines[id]
		if mm == nil {
			err = &cloudErr{404, "no such machine"}
			return
		}
		delete(st.Machines, id)
		st.Deletes++
		m = *mm
		m.Status = "deleted"
	})
	return
}

// ---- the fixture HTTP provider API (declarative row) ----

type fixtureAPI struct {
	*httptest.Server
	cloud *fakeCloud
	// key is the credential the API accepts.
	key string
}

func newFixtureAPI(t testing.TB, key string) *fixtureAPI {
	t.Helper()
	f := &fixtureAPI{cloud: newFakeCloud(""), key: key}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fixtureAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != f.key {
		// Echo what we got: a leak here must be caught by redaction.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad api key " + r.Header.Get("X-Api-Key")})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	fail := func(err error) {
		var ce *cloudErr
		if errors.As(err, &ce) {
			writeJSON(w, ce.status, map[string]string{"error": ce.msg})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
	}
	switch {
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "account" && r.Method == http.MethodGet:
		f.cloud.with(func(st *cloudState) { st.Calls++ })
		writeJSON(w, 200, map[string]any{"account": map[string]string{"id": "acct-fixture", "key_hint": f.key}})
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"machines": f.cloud.list()})
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodPost:
		var in struct {
			Name, Region, Size, Image string
			Tags                      []string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		m, stall, err := f.cloud.create(in.Name, in.Region, in.Size, in.Image, in.Tags)
		if err != nil {
			fail(err)
			return
		}
		if stall {
			<-r.Context().Done()
			return
		}
		writeJSON(w, 202, map[string]any{"machine": m})
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodGet:
		m, err := f.cloud.get(parts[2])
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, map[string]any{"machine": m})
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "machines" && r.Method == http.MethodDelete:
		m, err := f.cloud.remove(parts[2])
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, map[string]any{"machine": m})
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "machines" && parts[3] == "power" && r.Method == http.MethodPost:
		var in struct{ Action string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		m, err := f.cloud.power(parts[2], in.Action)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, map[string]any{"machine": m})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// declarativeManifest is the fixture's declarative package over api.
func declarativeManifest(baseURL string, caps []provider.Capability) provider.Manifest {
	doc := map[string]any{
		"base_url": baseURL,
		"auth":     map[string]string{"in": "header", "name": "X-Api-Key"},
		"headers":  map[string]string{"X-Bashy-Owner": "${owner_tag}"},
		"resource": map[string]string{
			"id": "id", "name": "name", "status": "status", "region": "region", "size": "size",
			"tags": "tags", "public_ips": "ips", "created_at": "created_at", "retained": "retained",
		},
		"states": map[string]string{"new": "pending", "running": "running", "stopped": "stopped", "deleted": "deleted"},
		"operations": map[string]any{
			"validate": map[string]any{"method": "GET", "path": "/api/account"},
			"create": map[string]any{"method": "POST", "path": "/api/machines", "result": "machine",
				"body": map[string]any{"name": "${name}", "region": "${spec.region}", "size": "${spec.size}", "image": "${spec.image}", "tags": "${tags}"}},
			"inspect": map[string]any{"method": "GET", "path": "/api/machines/${resource_id}", "result": "machine"},
			"list":    map[string]any{"method": "GET", "path": "/api/machines", "result": "machines"},
			"start":   map[string]any{"method": "POST", "path": "/api/machines/${resource_id}/power", "body": map[string]string{"action": "start"}, "result": "machine"},
			"stop":    map[string]any{"method": "POST", "path": "/api/machines/${resource_id}/power", "body": map[string]string{"action": "stop"}, "result": "machine"},
			"destroy": map[string]any{"method": "DELETE", "path": "/api/machines/${resource_id}", "result": "machine"},
		},
	}
	// Drop endpoints the manifest does not declare, so an undeclared op
	// has no endpoint either.
	ops := doc["operations"].(map[string]any)
	for op := range ops {
		if !slices.Contains(caps, provider.Capability(op)) {
			delete(ops, op)
		}
	}
	raw, _ := json.Marshal(doc)
	return provider.Manifest{
		Name: "fixture-decl", Kind: provider.KindInfra, Form: provider.FormDeclarative,
		ProtocolVersion: provider.ProtocolVersion, Capabilities: caps, Declarative: raw,
	}
}

// lifecycleCaps is the scenario's capability set; withoutStopStart the
// restricted one.
func lifecycleCaps() []provider.Capability {
	return []provider.Capability{"validate", "plan", "create", "inspect", "list", "start", "stop", "destroy"}
}

func withoutStopStart(caps []provider.Capability) []provider.Capability {
	var out []provider.Capability
	for _, c := range caps {
		if c != "start" && c != "stop" {
			out = append(out, c)
		}
	}
	return out
}

// ---- the executable fixture plugin (executable row) ----

// fixtureAdapter is the plugin's provider.Adapter over the persisted
// fixture cloud. It keeps the same discipline as the engines: lease tags,
// idempotent create by tag, owner check, A6 against the tags.
type fixtureAdapter struct {
	m     provider.Manifest
	cloud *fakeCloud
	owner string
}

func (f *fixtureAdapter) Manifest() provider.Manifest { return f.m }

func (m machine) resource() *provider.Resource {
	states := map[string]provider.State{"new": provider.StatePending, "running": provider.StateRunning, "stopped": provider.StateStopped, "deleted": provider.StateDeleted}
	st, ok := states[m.Status]
	if !ok {
		st = provider.StateUnknown
	}
	return &provider.Resource{ID: m.ID, Name: m.Name, State: st, RawStatus: m.Status, Region: m.Region, Size: m.Size, Tags: slices.Clone(m.Tags), PublicIPs: m.IPs, CreatedAt: m.CreatedAt}
}

func fixtureFail(op provider.Op, err error) (provider.Result, error) {
	var ce *cloudErr
	code := provider.ErrProvider
	retry := true
	if errors.As(err, &ce) {
		switch {
		case ce.status == 404:
			code, retry = provider.ErrNotFound, false
		case ce.status == 422:
			code, retry = provider.ErrInvalidRequest, false
		}
	}
	e := &provider.Error{Code: code, Message: "fixture: " + err.Error(), Retryable: retry}
	return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: op, Error: e}, e
}

func fixtureOK(op provider.Op, r *provider.Result) (provider.Result, error) {
	r.ProtocolVersion, r.Op = provider.ProtocolVersion, op
	return *r, nil
}

func (f *fixtureAdapter) findByKey(key string) *machine {
	tag := provider.IdempotencyTag(key)
	for _, m := range f.cloud.list() {
		if provider.Owned(m.Tags, f.owner) && slices.Contains(m.Tags, tag) {
			mm := m
			return &mm
		}
	}
	return nil
}

func (f *fixtureAdapter) owned(op provider.Op, id string) (*machine, provider.Result, error) {
	m, err := f.cloud.get(id)
	if err != nil {
		r, e := fixtureFail(op, err)
		return nil, r, e
	}
	if !provider.Owned(m.Tags, f.owner) {
		r, e := provider.Failure(op, provider.ErrNotOwned, "fixture: machine %s is not ours", id)
		return nil, r, e
	}
	return &m, provider.Result{}, nil
}

func (f *fixtureAdapter) Do(ctx context.Context, req provider.Request) (provider.Result, error) {
	switch req.Op {
	case provider.OpValidate:
		f.cloud.with(func(st *cloudState) { st.Calls++ })
		return fixtureOK(req.Op, &provider.Result{Data: json.RawMessage(`{"account":"fixture"}`)})
	case provider.OpPlan:
		data, _ := json.Marshal(map[string]any{"would_create": req.Spec, "tags": provider.LeaseTags(f.owner, req.IdempotencyKey, req.Authorization)})
		return fixtureOK(req.Op, &provider.Result{Data: data})
	case provider.OpCreate:
		if m := f.findByKey(req.IdempotencyKey); m != nil {
			return fixtureOK(req.Op, &provider.Result{Resource: m.resource()})
		}
		s := req.Spec
		tags := provider.LeaseTags(f.owner, req.IdempotencyKey, req.Authorization)
		tags = append(tags, s.Tags...)
		name := s.Name
		if name == "" {
			name = "bashy-" + req.IdempotencyKey
		}
		m, stall, err := f.cloud.create(name, s.Region, s.Size, s.Image, tags)
		if err != nil {
			return fixtureFail(req.Op, err)
		}
		if stall {
			// The side effect happened, the answer never arrived: the
			// plugin's own call to its cloud timed out after the request
			// was sent. It reports that as the normalized ambiguous
			// failure — the same classification the runner gives a plugin
			// it had to kill — so the scenario does not hinge on how fast
			// this process can be spawned and killed under load.
			e := &provider.Error{Code: provider.ErrTimeout, Message: "fixture: create sent; the provider never answered", Retryable: true}
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e}, e
		}
		return fixtureOK(req.Op, &provider.Result{Resource: m.resource()})
	case provider.OpInspect:
		m, r, err := f.owned(req.Op, req.ResourceID)
		if err != nil {
			return r, err
		}
		return fixtureOK(req.Op, &provider.Result{Resource: m.resource()})
	case provider.OpList:
		out := []provider.Resource{}
		for _, m := range f.cloud.list() {
			if provider.Owned(m.Tags, f.owner) {
				out = append(out, *m.resource())
			}
		}
		return fixtureOK(req.Op, &provider.Result{Resources: out})
	case provider.OpStart, provider.OpStop:
		m, r, err := f.owned(req.Op, req.ResourceID)
		if err != nil {
			return r, err
		}
		want, action := "running", "start"
		if req.Op == provider.OpStop {
			want, action = "stopped", "stop"
		}
		if m.Status != want {
			mm, err := f.cloud.power(req.ResourceID, action)
			if err != nil {
				return fixtureFail(req.Op, err)
			}
			m = &mm
		}
		res := &provider.Result{Resource: m.resource()}
		if req.Op == provider.OpStop {
			res.Cost = &provider.CostReport{Ongoing: true, Retained: append([]string{"resource:" + m.ID}, m.Retained...), Note: "stopped machines keep billing"}
		}
		return fixtureOK(req.Op, res)
	case provider.OpDestroy:
		m, r, err := f.owned(req.Op, req.ResourceID)
		if err != nil {
			return r, err
		}
		if e := provider.AuthorizeDestroy(req, *m.resource()); e != nil {
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
		}
		gone, err := f.cloud.remove(req.ResourceID)
		if err != nil {
			return fixtureFail(req.Op, err)
		}
		res := gone.resource()
		cost := &provider.CostReport{Ongoing: len(gone.Retained) > 0, Retained: gone.Retained}
		return fixtureOK(req.Op, &provider.Result{Resource: res, Cost: cost})
	}
	return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "fixture: %s", req.Op)
}

// fixturePluginMain is the plugin process: it serves one request from
// stdin over the persisted fixture cloud and exits. Called from TestMain.
func fixturePluginMain() {
	dir := os.Getenv(stateEnv)
	if dir == "" {
		os.Stderr.WriteString("fixture plugin: " + stateEnv + " is not set\n")
		os.Exit(2)
	}
	_ = os.WriteFile(filepath.Join(dir, "ran"), []byte("ran"), 0o600)
	// The frozen protocol carries no credential on stdin yet (S472.4a owns
	// the document); a plugin on user-owned compute resolves its own.
	cloud := newFakeCloud(filepath.Join(dir, "state.json"))
	a := &fixtureAdapter{m: executableManifest(dir, lifecycleCaps()), cloud: cloud, owner: "t1"}
	if err := provider.Serve(context.Background(), a, os.Stdin, os.Stdout); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// executableManifest is the fixture's executable package: this test
// binary in plugin mode (never a shell, never the bashy binary).
func executableManifest(stateDir string, caps []provider.Capability) provider.Manifest {
	return provider.Manifest{
		Name: "fixture-exec", Kind: provider.KindInfra, Form: provider.FormExecutable,
		ProtocolVersion: provider.ProtocolVersion, Capabilities: caps,
		Argv: []string{os.Args[0], "-test.run=^$"},
	}
}

// pluginEnvFor is the environment the dispatcher hands the plugin.
func pluginEnvFor(stateDir string) []string {
	return append(os.Environ(), pluginEnv+"=fixture", stateEnv+"="+stateDir)
}

// pluginRan reports whether a plugin process ever started under stateDir.
func pluginRan(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, "ran"))
	return err == nil
}
