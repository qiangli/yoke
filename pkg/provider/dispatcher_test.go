package provider_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

// doBuiltin registers the DigitalOcean reference adapter as the dispatcher's
// builtin for fake: the credential the dispatcher resolved is the token.
func doBuiltin(fake *fakedo.Server) provider.BuiltinFactory {
	return func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
		return digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: credential, Owner: "t1", PollInterval: time.Millisecond})
	}
}

func TestJournalResumeNoDuplicateCreate(t *testing.T) {
	fake := fakedo.New(t)
	store := provider.NewMemoryJournal()
	// crashing journals the intent, then "loses power" before the result
	// can be recorded: the create reached the provider, the journal did not
	// hear back.
	crashing := &crashAfterAppends{JournalStore: store, allow: 1}
	opts := provider.Options{
		Owner: "t1", Exec: provider.ExecDisabled, Journal: crashing,
		Credentials: provider.StaticCredential(fakedo.TestToken),
		Builtins:    map[string]provider.BuiltinFactory{digitalocean.Name: doBuiltin(fake)},
		Timeout:     2 * time.Second,
	}
	spec := &provider.Spec{Name: "sat", Region: "r1", Size: "s-1vcpu-1gb", Image: "img"}

	d1, err := provider.Open(context.Background(), digitalocean.Manifest(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d1.Do(context.Background(), provider.Request{Op: provider.OpCreate, IdempotencyKey: "boot-1", Spec: spec})
	if err == nil {
		t.Fatal("the simulated crash must surface as an error")
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount after the crash = %d, want 1", fake.CreateCount())
	}

	// Restart: a fresh dispatcher over the same durable store.
	opts.Journal = store
	d2, err := provider.Open(context.Background(), digitalocean.Manifest(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := d2.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reconciled != 1 || rep.NeedsManualReconcile != 0 {
		t.Fatalf("resume report %+v", rep)
	}
	res, err := d2.Do(context.Background(), provider.Request{Op: provider.OpCreate, IdempotencyKey: "boot-1", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource == nil || res.Resource.ID == "" {
		t.Fatalf("result %+v", res)
	}
	if n := fake.CreateCount(); n != 1 {
		t.Fatalf("CreateCount after resume = %d, want 1 (a duplicate allocation)", n)
	}
	if n := fake.DeleteCount(); n != 0 {
		t.Fatalf("resume deleted %d droplets, want 0", n)
	}
	if _, ok := fake.Droplet(atoiT(t, res.Resource.ID)); !ok {
		t.Fatalf("resumed resource %s is not the droplet the crashed create made", res.Resource.ID)
	}
}

// crashAfterAppends admits the first `allow` appends, then fails every
// later one, which leaves the journal exactly as a process killed between
// the provider call and the result record would.
type crashAfterAppends struct {
	provider.JournalStore
	allow int
}

func (c *crashAfterAppends) Append(ctx context.Context, e provider.JournalEntry) error {
	if c.allow <= 0 {
		return context.Canceled
	}
	c.allow--
	return c.JournalStore.Append(ctx, e)
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("not a number: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ---- the identical lifecycle scenario over every adapter form ----

// formRow is one adapter form under the dispatcher, with the fault hooks
// the scenario needs. The dispatcher is the only thing the scenario
// talks to; the rows differ only in how the provider is reached.
type formRow struct {
	name            string
	open            func(t *testing.T, opts provider.Options) *provider.Dispatcher
	setAsync        func(n int)
	stallNextCreate func()
	attachRetained  func(id string)
	exists          func(id string) bool
	creates         func() int
	deletes         func() int
	calls           func() int
}

// rows builds the built-in (DigitalOcean + fakedo), declarative (fixture
// HTTP API) and executable (fixture plugin) rows for caps.
func rows(t *testing.T, caps []provider.Capability) []formRow {
	t.Helper()
	fake := fakedo.New(t)
	doManifest := digitalocean.Manifest()
	doManifest.Capabilities = caps
	api := newFixtureAPI(t, fixtureKey)
	stateDir := t.TempDir()
	plugin := newFakeCloud(filepath.Join(stateDir, "state.json"))
	return []formRow{
		{
			name: "builtin-digitalocean",
			open: func(t *testing.T, opts provider.Options) *provider.Dispatcher {
				opts.Credentials = provider.StaticCredential(fakedo.TestToken)
				opts.Builtins = map[string]provider.BuiltinFactory{digitalocean.Name: doBuiltin(fake)}
				d, err := provider.Open(context.Background(), doManifest, opts)
				if err != nil {
					t.Fatal(err)
				}
				return d
			},
			setAsync:        fake.SetAsyncPolls,
			stallNextCreate: func() { fake.Inject("POST", "/v2/droplets", fakedo.StallAfterSideEffect, 1) },
			attachRetained:  func(id string) { fake.AttachVolume(atoiT(t, id), "vol-"+id) },
			exists:          func(id string) bool { _, ok := fake.Droplet(atoiT(t, id)); return ok },
			creates:         fake.CreateCount,
			deletes:         fake.DeleteCount,
			calls:           fake.RequestCount,
		},
		{
			name: "declarative-engine",
			open: func(t *testing.T, opts provider.Options) *provider.Dispatcher {
				opts.Credentials = provider.StaticCredential(fixtureKey)
				opts.Exec = provider.ExecDisabled
				opts.ExecFactory = func(provider.Manifest, provider.ExecOptions) (provider.Adapter, error) {
					t.Fatal("the declarative row must never build an exec adapter")
					return nil, nil
				}
				d, err := provider.Open(context.Background(), declarativeManifest(api.URL, caps), opts)
				if err != nil {
					t.Fatal(err)
				}
				return d
			},
			setAsync:        api.cloud.setAsync,
			stallNextCreate: api.cloud.stallNextCreate,
			attachRetained:  func(id string) { api.cloud.attachRetained(id, "disk-"+id) },
			exists:          api.cloud.exists,
			creates:         func() int { c, _, _ := api.cloud.counts(); return c },
			deletes:         func() int { _, d, _ := api.cloud.counts(); return d },
			calls:           func() int { _, _, c := api.cloud.counts(); return c },
		},
		{
			name: "executable-plugin",
			open: func(t *testing.T, opts provider.Options) *provider.Dispatcher {
				opts.Exec = provider.ExecAllowed
				opts.ExecEnv = pluginEnvFor(stateDir)
				// Each call spawns the test binary; its duration is the
				// host's to decide, so the budget is the test's own deadline,
				// not a guess at how fast a process starts under load. The
				// scenario's ambiguous create does not wait for this: the
				// fixture reports it (see fixtureAdapter.Do).
				opts.Timeout = execTimeout(t)
				d, err := provider.Open(context.Background(), executableManifest(stateDir, caps), opts)
				if err != nil {
					t.Fatal(err)
				}
				return d
			},
			setAsync:        plugin.setAsync,
			stallNextCreate: plugin.stallNextCreate,
			attachRetained:  func(id string) { plugin.attachRetained(id, "disk-"+id) },
			exists:          plugin.exists,
			creates:         func() int { c, _, _ := plugin.counts(); return c },
			deletes:         func() int { _, d, _ := plugin.counts(); return d },
			calls:           func() int { _, _, c := plugin.counts(); return c },
		},
	}
}

// execTimeout is the per-call budget for rows that spawn a process: a
// generous constant, or the remaining test deadline when that is shorter.
// Only tests that deliberately assert timeout behavior use a short one.
func execTimeout(t *testing.T) time.Duration {
	t.Helper()
	const generous = 60 * time.Second
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) / 2; left > 0 && left < generous {
			return left
		}
	}
	return generous
}

func baseOpts(j provider.JournalStore) provider.Options {
	return provider.Options{Owner: "t1", Journal: j, Timeout: 500 * time.Millisecond, Retries: 1}
}

func spec(name string) *provider.Spec {
	return &provider.Spec{Name: name, Region: "r1", Size: "s-1vcpu-1gb", Image: "img"}
}

func confirm(id, key string) *provider.Authorization {
	return &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: key}
}

func stateOf(res provider.Result) string {
	if res.Resource == nil {
		return "none"
	}
	return string(res.Resource.State)
}

// runLifecycle drives the scenario and returns its normalized transitions.
func runLifecycle(t *testing.T, r formRow, d *provider.Dispatcher) []string {
	t.Helper()
	ctx := context.Background()
	var tr []string
	step := func(s string) { tr = append(tr, s) }
	fatal := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %s: %v", r.name, what, err)
		}
	}

	// Async create: the resource is pending first, running later.
	r.setAsync(2)
	res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-a", Spec: spec("sat-a")})
	fatal("create", err)
	idA := res.Resource.ID
	step("create:" + stateOf(res))
	for i := 0; i < 10 && res.Resource.State != provider.StateRunning; i++ {
		res, err = d.Do(ctx, provider.Request{Op: provider.OpInspect, ResourceID: idA})
		fatal("inspect", err)
	}
	step("inspect:" + stateOf(res))

	// Duplicate request: same key, same resource, no second allocation.
	res, err = d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-a", Spec: spec("sat-a")})
	fatal("duplicate create", err)
	same := "different"
	if res.Resource != nil && res.Resource.ID == idA {
		same = "same"
	}
	step("create-dup:" + same)
	if n := r.creates(); n != 1 {
		t.Fatalf("%s: creates after duplicate = %d, want 1", r.name, n)
	}

	// Stop / start, or their explicit refusal when undeclared.
	res, err = d.Do(ctx, provider.Request{Op: provider.OpStop, IdempotencyKey: "stop-a", ResourceID: idA})
	if err != nil {
		step("stop:" + string(code(err)))
	} else {
		cost := "no-cost"
		if res.Cost != nil && res.Cost.Ongoing {
			cost = "ongoing"
		}
		step("stop:" + stateOf(res) + ":" + cost)
	}
	res, err = d.Do(ctx, provider.Request{Op: provider.OpStart, IdempotencyKey: "start-a", ResourceID: idA})
	if err != nil {
		step("start:" + string(code(err)))
	} else {
		step("start:" + stateOf(res))
	}

	// An operation outside the protocol never reaches the provider.
	before := r.calls()
	_, err = d.Do(ctx, provider.Request{Op: provider.Op("hibernate"), IdempotencyKey: "h-a", ResourceID: idA})
	step("hibernate:" + string(code(err)))
	if r.calls() != before {
		t.Fatalf("%s: an undeclared op reached the provider", r.name)
	}

	// Ambiguous create: the provider acts, the answer never arrives. The
	// dispatcher times out, reconciles by idempotency tag and finds it.
	r.stallNextCreate()
	res, err = d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-b", Spec: spec("sat-b")})
	fatal("ambiguous create", err)
	idB := res.Resource.ID
	step("create-ambiguous:recovered")
	if n := r.creates(); n != 2 {
		t.Fatalf("%s: creates after the ambiguous create = %d, want 2", r.name, n)
	}
	view, err := d.View(ctx)
	fatal("view", err)
	if op, ok := view.Find(provider.OpCreate, "k-b"); !ok || !op.Done || op.ResourceID != idB || op.Pending {
		t.Fatalf("%s: journal op k-b = %+v", r.name, op)
	}

	// Destroy without a confirmation: refused before the provider, the
	// resource stays active with its ongoing-cost report.
	before = r.calls()
	res, err = d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-a", ResourceID: idA})
	step("destroy-noconf:" + string(code(err)))
	if r.calls() != before || !r.exists(idA) {
		t.Fatalf("%s: refused destroy reached the provider or removed the resource", r.name)
	}
	if res.Cost == nil || !res.Cost.Ongoing {
		t.Fatalf("%s: refused destroy must report ongoing cost: %+v", r.name, res.Cost)
	}
	view, _ = d.View(ctx)
	if jr, ok := view.Resource(idA); !ok || jr.Destroyed || jr.Cost == nil || !jr.Cost.Ongoing {
		t.Fatalf("%s: journal after refused destroy: %+v", r.name, jr)
	}

	// Safe destroy with a per-operation confirmation: what the provider
	// keeps is reported.
	r.attachRetained(idA)
	res, err = d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-a", ResourceID: idA, Authorization: confirm(idA, "d-a")})
	fatal("confirmed destroy", err)
	retained := "nothing-retained"
	if res.Cost != nil && len(res.Cost.Retained) > 0 {
		retained = "retained"
	}
	step("destroy:" + stateOf(res) + ":" + retained)
	if r.exists(idA) || r.deletes() != 1 {
		t.Fatalf("%s: after confirmed destroy exists=%v deletes=%d", r.name, r.exists(idA), r.deletes())
	}
	res, err = d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-b", ResourceID: idB, Authorization: confirm(idB, "d-b")})
	fatal("destroy b", err)
	step("destroy-b:" + stateOf(res))
	return tr
}

func TestDispatcherSameScenarioBuiltinAndCustom(t *testing.T) {
	for _, caps := range []struct {
		name string
		caps []provider.Capability
	}{
		{"full", lifecycleCaps()},
		{"without-stop-start", withoutStopStart(lifecycleCaps())},
	} {
		t.Run(caps.name, func(t *testing.T) {
			var want []string
			var wantRow string
			for _, r := range rows(t, caps.caps) {
				d := r.open(t, baseOpts(provider.NewMemoryJournal()))
				got := runLifecycle(t, r, d)
				t.Logf("%s: %v", r.name, got)
				if want == nil {
					want, wantRow = got, r.name
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("transitions differ:\n%s: %v\n%s: %v", wantRow, want, r.name, got)
				}
			}
			if caps.name == "full" && !slices.Contains(want, "stop:stopped:ongoing") {
				t.Fatalf("full scenario did not stop: %v", want)
			}
			if caps.name == "without-stop-start" && !slices.Contains(want, "stop:unsupported_capability") {
				t.Fatalf("restricted scenario did not refuse stop: %v", want)
			}
		})
	}
}

func TestDeclarativeEngineNoExec(t *testing.T) {
	stateDir := t.TempDir()
	api := newFixtureAPI(t, fixtureKey)
	opts := baseOpts(provider.NewMemoryJournal())
	opts.Credentials = provider.StaticCredential(fixtureKey)
	opts.Exec = provider.ExecDisabled
	opts.ExecEnv = pluginEnvFor(stateDir)
	opts.ExecFactory = func(provider.Manifest, provider.ExecOptions) (provider.Adapter, error) {
		t.Fatal("exec hook called for a declarative manifest")
		return nil, nil
	}
	d, err := provider.Open(context.Background(), declarativeManifest(api.URL, lifecycleCaps()), opts)
	if err != nil {
		t.Fatal(err)
	}
	r := rows(t, lifecycleCaps())[1]
	r.setAsync, r.stallNextCreate = api.cloud.setAsync, api.cloud.stallNextCreate
	r.attachRetained = func(id string) { api.cloud.attachRetained(id, "disk") }
	r.exists = api.cloud.exists
	r.creates = func() int { c, _, _ := api.cloud.counts(); return c }
	r.deletes = func() int { _, d, _ := api.cloud.counts(); return d }
	r.calls = func() int { _, _, c := api.cloud.counts(); return c }
	got := runLifecycle(t, r, d)
	if !slices.Contains(got, "create-ambiguous:recovered") || pluginRan(stateDir) {
		t.Fatalf("transitions %v, plugin ran: %v", got, pluginRan(stateDir))
	}
}

func TestExecDisabledRefusesExecutablePlugin(t *testing.T) {
	stateDir := t.TempDir()
	opts := baseOpts(provider.NewMemoryJournal())
	opts.Exec = provider.ExecDisabled
	opts.ExecEnv = pluginEnvFor(stateDir)
	opts.ExecFactory = func(provider.Manifest, provider.ExecOptions) (provider.Adapter, error) {
		t.Fatal("exec factory called under ExecDisabled")
		return nil, nil
	}
	_, err := provider.Open(context.Background(), executableManifest(stateDir, lifecycleCaps()), opts)
	var e *provider.Error
	if !errors.As(err, &e) || e.Code != provider.ErrExecDisabled {
		t.Fatalf("got %v, want a structured %s", err, provider.ErrExecDisabled)
	}
	if pluginRan(stateDir) {
		t.Fatal("a plugin process started")
	}
	// The same manifest opens where exec is allowed.
	opts.Exec, opts.ExecFactory = provider.ExecAllowed, nil
	if _, err := provider.Open(context.Background(), executableManifest(stateDir, lifecycleCaps()), opts); err != nil {
		t.Fatal(err)
	}
	if pluginRan(stateDir) {
		t.Fatal("Open must not start the plugin")
	}
}

func TestCredentialRedactedFromJournal(t *testing.T) {
	const planted = "planted-secret-ZqX1-do-not-leak"
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newFixtureAPI(t, planted)
	var logs []string
	opts := baseOpts(nil) // nil journal: the file store under BASHY_HOME
	opts.Credentials = provider.StaticCredential(planted)
	opts.Log = func(line string) { logs = append(logs, line) }
	opts.Redact = func(s string) string { return strings.ReplaceAll(s, "sat-host-77", "[host]") }
	d, err := provider.Open(context.Background(), declarativeManifest(api.URL, lifecycleCaps()), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A label and a spec name that carry the secret (an agent pasting a
	// token where it does not belong) and an ambiguous create (a log line).
	api.cloud.stallNextCreate()
	s := spec("sat-host-77")
	s.Labels = map[string]string{"token": planted}
	s.UserData = []byte("#cloud-config\ntoken: " + planted)
	res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-" + planted, Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Resource.ID
	if _, err := d.Do(ctx, provider.Request{Op: provider.OpStop, IdempotencyKey: "s1", ResourceID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d1", ResourceID: id, Authorization: confirm(id, "d1")}); err != nil {
		t.Fatal(err)
	}
	// A wrong credential: the fixture echoes it in its 401, which lands in
	// a journaled error message.
	opts.Credentials = provider.StaticCredential("wrong-" + planted)
	d2, err := provider.Open(ctx, declarativeManifest(api.URL, lifecycleCaps()), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k2", Spec: spec("x")}); code(err) != provider.ErrInvalidCredentials {
		t.Fatalf("wrong credential: %v", err)
	}
	if _, _, calls := api.cloud.counts(); calls == 0 {
		t.Fatal("the credential never reached the provider API")
	}

	dir, err := provider.DefaultJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, os.Getenv("BASHY_HOME")) {
		t.Fatalf("journal dir %s is not under BASHY_HOME", dir)
	}
	var raw []byte
	entries, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range entries {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
	}
	if len(raw) == 0 {
		t.Fatal("no journal written")
	}
	for _, what := range []struct{ name, text string }{{"journal", string(raw)}, {"logs", strings.Join(logs, "\n")}} {
		if strings.Contains(what.text, planted) {
			t.Fatalf("%s contains the planted credential:\n%s", what.name, what.text)
		}
	}
	if !strings.Contains(string(raw), "[REDACTED:credential]") {
		t.Fatalf("journal never needed redaction; the test proved nothing:\n%s", raw)
	}
	if strings.Contains(string(raw), "sat-host-77") || !strings.Contains(string(raw), "[host]") {
		t.Fatalf("the caller's redactor was not applied:\n%s", raw)
	}
	if strings.Contains(string(raw), "cloud-config") {
		t.Fatalf("user_data was journaled:\n%s", raw)
	}
	if len(logs) == 0 {
		t.Fatal("no log lines; the ambiguous create should have logged")
	}

	// The credential may never ride the executable plugin's environment.
	stateDir := t.TempDir()
	eo := baseOpts(provider.NewMemoryJournal())
	eo.Exec = provider.ExecAllowed
	eo.Credentials = provider.StaticCredential(planted)
	eo.ExecEnv = append(pluginEnvFor(stateDir), "PROVIDER_TOKEN="+planted)
	if _, err := provider.Open(ctx, executableManifest(stateDir, lifecycleCaps()), eo); err != nil {
		// The executable form resolves no credential in the dispatcher,
		// so an env entry cannot be checked against one; that is fine.
		t.Logf("exec open: %v", err)
	}
	if pluginRan(stateDir) {
		t.Fatal("Open started the plugin")
	}
}

func TestDestroyRequiresPerOperationConfirmation(t *testing.T) {
	for _, r := range rows(t, lifecycleCaps()) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			d := r.open(t, baseOpts(provider.NewMemoryJournal()))
			mk := func(key string) string {
				res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec(key)})
				if err != nil {
					t.Fatal(err)
				}
				return res.Resource.ID
			}
			a, b := mk("k-a"), mk("k-b")
			cases := map[string]*provider.Authorization{
				"none":                    nil,
				"bound to other resource": confirm(b, "d-1"),
				"bound to other key":      confirm(a, "d-0"),
				"temporary on persistent": {Kind: provider.AuthTemporary},
				"unknown kind":            {Kind: "maybe", ResourceID: a, IdempotencyKey: "d-1"},
			}
			for name, auth := range cases {
				before := r.calls()
				res, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-1", ResourceID: a, Authorization: auth})
				if code(err) != provider.ErrConfirmationRequired {
					t.Fatalf("%s: got %v, want confirmation_required", name, err)
				}
				if res.Cost == nil || !res.Cost.Ongoing {
					t.Fatalf("%s: refused destroy must report ongoing cost", name)
				}
				if r.calls() != before {
					t.Fatalf("%s: the refusal reached the provider", name)
				}
			}
			if r.deletes() != 0 || !r.exists(a) {
				t.Fatalf("something was deleted: deletes=%d exists=%v", r.deletes(), r.exists(a))
			}
			view, _ := d.View(ctx)
			if jr, _ := view.Resource(a); jr.Destroyed || jr.Cost == nil || !jr.Cost.Ongoing {
				t.Fatalf("journal must keep %s active with its cost: %+v", a, jr)
			}

			// The bound confirmation goes through, once.
			if _, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-1", ResourceID: a, Authorization: confirm(a, "d-1")}); err != nil || r.deletes() != 1 {
				t.Fatalf("confirmed destroy: err=%v deletes=%d", err, r.deletes())
			}
			// Reused for a second resource: refused, no provider call.
			before := r.calls()
			_, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-1", ResourceID: b, Authorization: confirm(a, "d-1")})
			if code(err) != provider.ErrConfirmationRequired && code(err) != provider.ErrInvalidRequest {
				t.Fatalf("reused confirmation: %v", err)
			}
			// The key itself carried to another resource: refused too.
			_, err = d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-1", ResourceID: b, Authorization: confirm(b, "d-1")})
			if code(err) != provider.ErrInvalidRequest {
				t.Fatalf("reused key: %v", err)
			}
			// Replayed on the same resource: answered from the journal.
			res, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-1", ResourceID: a, Authorization: confirm(a, "d-1")})
			if err != nil || res.Resource == nil || res.Resource.State != provider.StateDeleted {
				t.Fatalf("replay: err=%v res=%+v", err, res)
			}
			if r.calls() != before || r.deletes() != 1 || !r.exists(b) {
				t.Fatalf("reuse reached the provider: calls=%d/%d deletes=%d b exists=%v", r.calls(), before, r.deletes(), r.exists(b))
			}
			// A fresh confirmation for b with its own key is what destroys b.
			if _, err := d.Do(ctx, provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d-2", ResourceID: b, Authorization: confirm(b, "d-2")}); err != nil || r.deletes() != 2 {
				t.Fatalf("destroy b: err=%v deletes=%d", err, r.deletes())
			}
		})
	}
}

func TestTemporaryResourceDestroyNoPrompt(t *testing.T) {
	for _, r := range rows(t, lifecycleCaps()) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			opts := baseOpts(provider.NewMemoryJournal())
			opts.Now = func() time.Time { return now }
			d := r.open(t, opts)
			mk := func(key string, auth *provider.Authorization) string {
				res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec(key), Authorization: auth})
				if err != nil {
					t.Fatal(err)
				}
				return res.Resource.ID
			}
			persistent := mk("k-p", nil)
			ttl := mk("k-t", &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: now.Add(time.Hour)})
			work := mk("k-w", &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job-1"})

			sweep := func() int {
				out, err := d.ExpireTemporary(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, res := range out {
					if res.Error != nil {
						t.Fatalf("sweep result %+v", res.Error)
					}
				}
				return len(out)
			}
			if n := sweep(); n != 0 || r.deletes() != 0 {
				t.Fatalf("before expiry: swept %d, deletes %d", n, r.deletes())
			}
			now = now.Add(2 * time.Hour)
			if n := sweep(); n != 1 || r.deletes() != 1 || r.exists(ttl) {
				t.Fatalf("at expiry: swept %d, deletes %d, exists %v", n, r.deletes(), r.exists(ttl))
			}
			before := r.calls()
			if n := sweep(); n != 0 || r.deletes() != 1 || r.calls() != before {
				t.Fatalf("second sweep: swept %d, deletes %d, calls %d/%d", n, r.deletes(), r.calls(), before)
			}
			view, _ := d.View(ctx)
			op, ok := view.Find(provider.OpDestroy, provider.TemporaryKey("k-t"))
			if !ok || !op.Done || op.Attempts != 1 {
				t.Fatalf("journaled TTL destroy: %+v", op)
			}
			if jr, _ := view.Resource(ttl); !jr.Destroyed || jr.Temporary == nil {
				t.Fatalf("journaled TTL resource: %+v", jr)
			}

			// Work completion destroys the work-bound one, once.
			out, err := d.CompleteWork(ctx, "job-1")
			if err != nil || len(out) != 1 || out[0].Error != nil || r.deletes() != 2 || r.exists(work) {
				t.Fatalf("work completion: err=%v out=%+v deletes=%d", err, out, r.deletes())
			}
			if out, _ := d.CompleteWork(ctx, "job-1"); len(out) != 0 || r.deletes() != 2 {
				t.Fatalf("second work completion: %+v deletes=%d", out, r.deletes())
			}
			if !r.exists(persistent) {
				t.Fatal("the persistent resource was destroyed by a sweep")
			}
		})
	}
}

func TestJournalNeedsManualReconcileWhenUnprovable(t *testing.T) {
	ctx := context.Background()
	api := newFixtureAPI(t, fixtureKey)
	store := provider.NewMemoryJournal()
	opts := baseOpts(&crashAfterAppends{JournalStore: store, allow: 1})
	opts.Credentials = provider.StaticCredential(fixtureKey)
	d1, err := provider.Open(ctx, declarativeManifest(api.URL, lifecycleCaps()), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d1.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: spec("x")}); err == nil {
		t.Fatal("crash must surface")
	}
	// The provider is unreachable at restart: the outcome cannot be proven.
	api.Close()
	opts.Journal = store
	d2, err := provider.Open(ctx, declarativeManifest(api.URL, lifecycleCaps()), opts)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := d2.Resume(ctx)
	if err != nil || rep.NeedsManualReconcile != 1 || rep.Reconciled != 0 {
		t.Fatalf("resume: err=%v rep=%+v", err, rep)
	}
	view, _ := d2.View(ctx)
	if len(view.ManualReconcile()) != 1 {
		t.Fatalf("view %+v", view.Operations)
	}
	if _, err := d2.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k", Spec: spec("x")}); code(err) != provider.ErrNeedsManualReconcile {
		t.Fatalf("retry under manual reconcile: %v", err)
	}
}

// ---- a create replay is proven, not believed (S522.2 rework 4) ----

// The live bug: `infra destroy --name zoo-aws` removed the host, and the
// next `infra provision` with the same default key printed "provisioned
// zoo-aws (running)" while making no provider call at all — the journal
// replayed a create whose instance no longer existed. A destroy and a
// create cannot be made to share one key (they are different operations),
// so the create key's answer has to be retired when its resource goes.
func TestCreateAfterDestroyWithTheSameKeyReallyCreates(t *testing.T) {
	for _, r := range rows(t, lifecycleCaps()) {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			d := r.open(t, baseOpts(provider.NewMemoryJournal()))
			const key = "infra-provision-zoo"
			first, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("zoo")})
			if err != nil {
				t.Fatal(err)
			}
			id := first.Resource.ID
			if _, err := d.Do(ctx, provider.Request{
				Op: provider.OpDestroy, IdempotencyKey: "infra-destroy-zoo",
				ResourceID: id, Authorization: confirm(id, "infra-destroy-zoo"),
			}); err != nil {
				t.Fatal(err)
			}
			if r.exists(id) {
				t.Fatalf("%s still exists after the destroy", id)
			}
			second, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("zoo")})
			if err != nil {
				t.Fatalf("create after destroy: %v", err)
			}
			if n := r.creates(); n != 2 {
				t.Fatalf("creates = %d, want 2: the replay reported a host nobody created", n)
			}
			if second.Resource == nil || !r.exists(second.Resource.ID) {
				t.Fatalf("create reported %+v, which does not exist", second.Resource)
			}
			if second.Resource.ID == id {
				t.Fatalf("create reported the destroyed resource %s", id)
			}
			// The journal says what happened: the first answer retired,
			// the second create is the key's answer now.
			view, verr := d.View(ctx)
			if verr != nil {
				t.Fatal(verr)
			}
			op, ok := view.Find(provider.OpCreate, key)
			if !ok || op.Retired {
				t.Fatalf("create op = %+v, want a live (not retired) answer", op)
			}
			if op.ResourceID != second.Resource.ID {
				t.Fatalf("op resource = %q, want %q", op.ResourceID, second.Resource.ID)
			}
			if jr, known := view.Resource(id); !known || !jr.Destroyed {
				t.Fatalf("journal resource %s = %+v, want destroyed", id, jr)
			}
		})
	}
}

// Same hazard without a destroy on this host at all: the resource was
// removed in the provider's own console. The journal cannot know, so the
// replay is proven against the provider before it is served.
func TestCreateReplayRetiresAResourceThatIsGoneAtTheProvider(t *testing.T) {
	ctx := context.Background()
	fake := fakedo.New(t)
	store := provider.NewMemoryJournal()
	const key = "infra-provision-ghost"
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	// A completed create of droplet 424242, which no longer exists.
	for _, e := range []provider.JournalEntry{
		{At: at, Provider: digitalocean.Name, Op: provider.OpCreate, IdempotencyKey: key, Phase: provider.PhaseIntent, Attempt: 1},
		{At: at.Add(time.Second), Provider: digitalocean.Name, Op: provider.OpCreate, IdempotencyKey: key, Phase: provider.PhaseResult, Attempt: 1,
			ResultID: "424242", State: provider.StateRunning, Tags: provider.LeaseTags("t1", key, nil)},
	} {
		if err := store.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	opts := baseOpts(store)
	opts.Credentials = provider.StaticCredential(fakedo.TestToken)
	opts.Builtins = map[string]provider.BuiltinFactory{digitalocean.Name: doBuiltin(fake)}
	d, err := provider.Open(ctx, digitalocean.Manifest(), opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("ghost")})
	if err != nil {
		t.Fatal(err)
	}
	if n := fake.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1: a create whose resource is gone must create", n)
	}
	if res.Resource == nil || res.Resource.ID == "424242" {
		t.Fatalf("resource = %+v, want the newly created droplet", res.Resource)
	}
	entries, err := store.Load(ctx, digitalocean.Name)
	if err != nil {
		t.Fatal(err)
	}
	var retired *provider.JournalEntry
	for i := range entries {
		if entries[i].Phase == provider.PhaseRetired {
			retired = &entries[i]
		}
	}
	if retired == nil {
		t.Fatal("no retirement was journaled; the dead answer could come back")
	}
	if retired.ResourceID != "424242" || retired.Op != provider.OpCreate || retired.IdempotencyKey != key {
		t.Fatalf("retirement = %+v", retired)
	}
}

// An inspect that merely FAILS proves nothing: a provider that is down or
// throttling must not turn a replay into a second host. The journal's
// answer stands and nothing is created.
func TestCreateReplayKeepsTheJournalAnswerWhenTheProofFails(t *testing.T) {
	ctx := context.Background()
	fake := fakedo.New(t)
	id := fake.AddDroplet("live", provider.LeaseTags("t1", "infra-provision-live", nil)...)
	store := provider.NewMemoryJournal()
	const key = "infra-provision-live"
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for _, e := range []provider.JournalEntry{
		{At: at, Provider: digitalocean.Name, Op: provider.OpCreate, IdempotencyKey: key, Phase: provider.PhaseIntent, Attempt: 1},
		{At: at.Add(time.Second), Provider: digitalocean.Name, Op: provider.OpCreate, IdempotencyKey: key, Phase: provider.PhaseResult, Attempt: 1,
			ResultID: strconv.Itoa(id), State: provider.StateRunning, Tags: provider.LeaseTags("t1", key, nil)},
	} {
		if err := store.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	opts := baseOpts(store)
	opts.Credentials = provider.StaticCredential(fakedo.TestToken)
	opts.Builtins = map[string]provider.BuiltinFactory{digitalocean.Name: doBuiltin(fake)}
	d, err := provider.Open(ctx, digitalocean.Manifest(), opts)
	if err != nil {
		t.Fatal(err)
	}
	fake.Inject("GET", "/v2/droplets/"+strconv.Itoa(id), fakedo.Fail5xx, 1)
	res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("live")})
	if err != nil {
		t.Fatalf("replay must survive a failed proof: %v", err)
	}
	if n := fake.CreateCount(); n != 0 {
		t.Fatalf("CreateCount = %d, want 0: an unproven absence created a second host", n)
	}
	if res.Resource == nil || res.Resource.ID != strconv.Itoa(id) {
		t.Fatalf("resource = %+v, want the journaled droplet %d", res.Resource, id)
	}
	// A live resource is reported as it is NOW, not as the journal
	// remembers it: the proof is a read, so its answer is the fresh one.
	res, err = d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: key, Spec: spec("live")})
	if err != nil {
		t.Fatal(err)
	}
	drop, _ := fake.Droplet(id)
	if res.Resource == nil || res.Resource.RawStatus != drop.Status {
		t.Fatalf("resource = %+v, want the droplet's current status %q", res.Resource, drop.Status)
	}
	if n := fake.CreateCount(); n != 0 {
		t.Fatalf("CreateCount = %d, want 0", n)
	}
}

// slowAdapter answers every operation after delay, so a test can watch
// which per-call budget an operation was given.
type slowAdapter struct {
	m     provider.Manifest
	delay time.Duration
}

func (s *slowAdapter) Manifest() provider.Manifest { return s.m }

func (s *slowAdapter) Do(ctx context.Context, req provider.Request) (provider.Result, error) {
	select {
	case <-ctx.Done():
		e := &provider.Error{Code: provider.ErrTimeout, Message: "slow: " + ctx.Err().Error(), Retryable: true}
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e}, e
	case <-time.After(s.delay):
	}
	res := provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op}
	if req.Op == provider.OpCreate || req.Op == provider.OpInspect {
		res.Resource = &provider.Resource{
			ID: "slow-1", Name: "slow-1", State: provider.StateRunning,
			Tags: provider.LeaseTags("t1", req.IdempotencyKey, nil),
		}
	}
	return res, nil
}

// A read answers in a second or it is broken; a create that brings a host
// all the way to its serving shape (AWS Lightsail waits for "running",
// then applies the firewall and the static IP) legitimately runs for
// minutes. Bounding the create at the read budget cancels the adapter
// mid-sequence, and the dispatcher then reconciles it as "the resource
// exists" — success for a host whose remaining steps never ran, which is
// how the live zoo-aws came up with Lightsail's default firewall.
func TestMutatingTimeoutIsSeparateFromTheReadTimeout(t *testing.T) {
	ctx := context.Background()
	m := digitalocean.Manifest()
	m.Name = "slow"
	open := func(mutating time.Duration) *provider.Dispatcher {
		opts := provider.Options{
			Owner: "t1", Journal: provider.NewMemoryJournal(),
			Timeout: 20 * time.Millisecond, MutatingTimeout: mutating,
			Credentials: provider.StaticCredential("t"),
			Builtins: map[string]provider.BuiltinFactory{"slow": func(context.Context, provider.Manifest, string) (provider.Adapter, error) {
				return &slowAdapter{m: m, delay: 150 * time.Millisecond}, nil
			}},
		}
		d, err := provider.Open(ctx, m, opts)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// Without the larger budget the create is cancelled at 20ms, and
	// what the caller gets is the mess that follows: the outcome is
	// unprovable because the reconciling read is cancelled too.
	_, err := open(0).Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k1", Spec: spec("slow-1")})
	switch code(err) {
	case provider.ErrTimeout, provider.ErrNeedsManualReconcile:
	default:
		t.Fatalf("create err = %v, want it cancelled at the read budget", err)
	}
	d := open(5 * time.Second)
	res, err := d.Do(ctx, provider.Request{Op: provider.OpCreate, IdempotencyKey: "k2", Spec: spec("slow-1")})
	if err != nil {
		t.Fatalf("create under the mutating budget: %v", err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateRunning {
		t.Fatalf("resource = %+v", res.Resource)
	}
	// A read keeps the read budget: the larger one is not a licence for
	// every call to hang.
	if _, err := d.Do(ctx, provider.Request{Op: provider.OpInspect, ResourceID: "slow-1"}); code(err) != provider.ErrTimeout {
		t.Fatalf("inspect err = %v, want timeout at the read budget", err)
	}
}
