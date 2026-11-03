package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/conformance"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
	"github.com/qiangli/yoke/pkg/provider/scaffold"
)

const (
	confOwner = "conformance"
	confKey   = "conformance-fixture-key"
)

// declarativeSetup scaffolds a declarative provider in dir, points its
// manifest at fake, and opens it the way the master links the dispatcher:
// exec disabled, memory journal, no network beyond the fake.
func declarativeSetup(t *testing.T, dir string, fake *conformance.FakeAPI) conformance.Setup {
	t.Helper()
	m := loadWithBaseURL(t, dir, fake.URL)
	d, err := provider.Open(context.Background(), m, provider.Options{
		Owner:       confOwner,
		Exec:        provider.ExecDisabled,
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential(confKey),
		Timeout:     5 * time.Second,
		Retries:     1,
		ExecFactory: func(provider.Manifest, provider.ExecOptions) (provider.Adapter, error) {
			t.Error("exec factory called for a declarative manifest")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conformance.Setup{
		Dispatcher: d,
		Counter:    fake,
		RunningID:  fake.RunningID,
		StoppedID:  fake.StoppedID,
		TempID:     fake.TempID,
		InjectCreateLoss: func() {
			fake.FailNextCreateAfterSideEffect()
		},
	}
}

func loadWithBaseURL(t *testing.T, dir, url string) provider.Manifest {
	t.Helper()
	m, err := scaffold.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(m.Declarative, &doc); err != nil {
		t.Fatal(err)
	}
	doc["base_url"] = url
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	m.Declarative = raw
	return m
}

// executableSetup opens the scaffold's plugin.sh the way user-owned compute
// does: exec allowed, working directory inside the scaffold so ./plugin.sh
// resolves, every spawn counted. The plugin starts stateless, so the
// fixtures are provisioned through the dispatcher first.
func executableSetup(t *testing.T, dir string, _ *conformance.FakeAPI) conformance.Setup {
	t.Helper()
	m, err := scaffold.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var spawns atomic.Int64
	d, err := provider.Open(context.Background(), m, provider.Options{
		Owner:   confOwner,
		Exec:    provider.ExecAllowed,
		Journal: provider.NewMemoryJournal(),
		Timeout: 10 * time.Second,
		Retries: 1,
		ExecDir: dir,
		ExecFactory: func(m provider.Manifest, opts provider.ExecOptions) (provider.Adapter, error) {
			spawns.Add(1)
			return provider.NewExecAdapter(m, opts)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := d.Do(ctx, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "seed-running",
		Spec: &provider.Spec{Name: "fx-running", Region: "region-1", Size: "small", Image: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := d.Do(ctx, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "seed-stopped",
		Spec: &provider.Spec{Name: "fx-stopped", Region: "region-1", Size: "small", Image: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.Resource == nil || stopped.Resource == nil {
		t.Fatalf("seed creates returned no resources: %+v %+v", running, stopped)
	}
	if _, err := d.Do(ctx, provider.Request{
		Op: provider.OpStop, IdempotencyKey: "seed-park-stopped", ResourceID: stopped.Resource.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return conformance.Setup{
		Dispatcher: d,
		Counter:    conformance.SpawnCounter(&spawns),
		RunningID:  running.Resource.ID,
		StoppedID:  stopped.Resource.ID,
	}
}

// First red test (S472.14): a scaffold generated in t.TempDir() and
// completed with the fixture implementation must pass validate and the
// full conformance suite against its fake API; any failing case fails
// the test.
func TestScaffoldToConformanceNoSourceChange(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.FormDeclarative, Name: "scaffold-fixture"}); err != nil {
		t.Fatalf("scaffold.Init failed: %v", err)
	}
	if err := scaffold.Validate(dir); err != nil {
		t.Fatalf("scaffold.Validate failed: %v", err)
	}
	fake := conformance.NewFakeAPI(t, confKey, confOwner)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return declarativeSetup(t, dir, fake)
	}, fake)
	// And again with the allocating lifecycle enabled: the scaffold
	// passes the live checks too, including ambiguous-create recovery.
	live := conformance.NewFakeAPI(t, confKey, confOwner)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return declarativeSetup(t, dir, live)
	}, live, conformance.WithLive("scaffold-live"))
	if n := live.Losses(); n != 1 {
		t.Fatalf("injected lost responses consumed = %d, want 1 (the ambiguous-create check must fire its fault)", n)
	}
}

// A declarative scaffold passes validate and the full suite with exec
// disabled (the master's configuration); an exec hook fails the test if
// called, and an executable manifest is refused without ever spawning.
func TestConformanceDeclarativeOnMasterEngine(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.FormDeclarative, Name: "master-decl"}); err != nil {
		t.Fatal(err)
	}
	if err := scaffold.Validate(dir); err != nil {
		t.Fatal(err)
	}
	fake := conformance.NewFakeAPI(t, confKey, confOwner)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return declarativeSetup(t, dir, fake)
	}, fake)

	execDir := t.TempDir()
	if err := scaffold.Init(scaffold.Options{Dir: execDir, Form: provider.FormExecutable, Name: "master-exec"}); err != nil {
		t.Fatal(err)
	}
	m, err := scaffold.Load(execDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Open(context.Background(), m, provider.Options{
		Owner:   confOwner,
		Exec:    provider.ExecDisabled,
		Journal: provider.NewMemoryJournal(),
		ExecFactory: func(provider.Manifest, provider.ExecOptions) (provider.Adapter, error) {
			t.Error("exec factory called under ExecDisabled")
			return nil, nil
		},
	})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.ErrExecDisabled {
		t.Fatalf("got %v, want a structured %s", err, provider.ErrExecDisabled)
	}
}

// The fixture-only suite allocates nothing: against the DigitalOcean
// adapter over fakedo, CreateCount stays 0 unless live mode is enabled
// with an explicit resource authorization.
func TestConformanceAllocatesNothingByDefault(t *testing.T) {
	fake := fakedo.New(t)
	seeds := seedFakedo(t, fake)
	factory := func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return doSetup(t, fake, seeds)
	}
	conformance.Run(t, factory, nil)
	if n := fake.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after the fixture-only suite = %d, want 0", n)
	}

	live := fakedo.New(t)
	liveSeeds := seedFakedo(t, live)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return doSetup(t, live, liveSeeds)
	}, nil, conformance.WithLive("test-alloc-ok"))
	if n := live.CreateCount(); n == 0 {
		t.Fatal("CreateCount after the live suite = 0, want at least one allocation")
	}
}

type doSeeds struct{ running, stopped, temp string }

func doSetup(t *testing.T, fake *fakedo.Server, seeds doSeeds) conformance.Setup {
	t.Helper()
	manifest := digitalocean.Manifest()
	d, err := provider.Open(context.Background(), manifest, provider.Options{
		Owner:       "t1",
		Exec:        provider.ExecDisabled,
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential(fakedo.TestToken),
		Builtins: map[string]provider.BuiltinFactory{
			digitalocean.Name: func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
				return digitalocean.New(digitalocean.Config{
					BaseURL: fake.URL, Token: credential, Owner: "t1", PollInterval: time.Millisecond,
				})
			},
		},
		Timeout: 5 * time.Second,
		Retries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Park the stopped fixture in off state through the adapter (a stop
	// is not an allocation, so the fixture-only budget is unaffected).
	if _, err := d.Do(context.Background(), provider.Request{
		Op: provider.OpStop, IdempotencyKey: "seed-park-stopped", ResourceID: seeds.stopped,
	}); err != nil {
		t.Fatal(err)
	}
	return conformance.Setup{
		Dispatcher: d,
		Counter:    doCounter{fake},
		RunningID:  seeds.running,
		StoppedID:  seeds.stopped,
		TempID:     seeds.temp,
		InjectCreateLoss: func() {
			fake.Inject("POST", "/v2/droplets", fakedo.LoseResponse, 1)
		},
	}
}

// seedFakedo provisions deterministic fixtures directly in the fake's
// state (no provider call, no allocation counted): two running droplets
// and a temporary one carrying TTL tags.
func seedFakedo(t *testing.T, fake *fakedo.Server) doSeeds {
	t.Helper()
	running := provider.LeaseTags("t1", "seed-running", nil)
	stopped := provider.LeaseTags("t1", "seed-stopped", nil)
	temp := provider.LeaseTags("t1", "seed-temp", &provider.Authorization{
		Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour),
	})
	return doSeeds{
		running: fmt.Sprint(fake.AddDroplet("fx-running", running...)),
		stopped: fmt.Sprint(fake.AddDroplet("fx-stopped", stopped...)),
		temp:    fmt.Sprint(fake.AddDroplet("fx-temp", temp...)),
	}
}

type doCounter struct{ s *fakedo.Server }

func (f doCounter) CreateCount() int  { return f.s.CreateCount() }
func (f doCounter) DeleteCount() int  { return f.s.DeleteCount() }
func (f doCounter) RequestCount() int { return f.s.RequestCount() }

func (f doCounter) Exists(id string) bool {
	n := 0
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	_, ok := f.s.Droplet(n)
	return ok
}

// A deliberately wrong fixture must fail the suite, in both forms: its
// stop reports the resource deleted (destroy semantics on a stop path).
func TestConformanceDetectsDestroyForStop(t *testing.T) {
	t.Run("declarative", func(t *testing.T) {
		dir := t.TempDir()
		if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.FormDeclarative, Name: "wrong-decl"}); err != nil {
			t.Fatal(err)
		}
		fake := conformance.NewFakeAPI(t, confKey, confOwner)
		fake.SetStopDeletes(true)
		err := conformance.Check(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
			return declarativeSetup(t, dir, fake)
		}, fake)
		if err == nil {
			t.Fatal("a fixture whose stop deletes must fail conformance")
		}
		t.Logf("detected: %v", err)
	})

	t.Run("executable", func(t *testing.T) {
		dir := t.TempDir()
		if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.FormExecutable, Name: "wrong-exec"}); err != nil {
			t.Fatal(err)
		}
		rewritePluginStop(t, dir)
		err := conformance.Check(t, func(t *testing.T, fake *conformance.FakeAPI) conformance.Setup {
			return executableSetup(t, dir, fake)
		}, nil)
		if err == nil {
			t.Fatal("a plugin whose stop deletes must fail conformance")
		}
		t.Logf("detected: %v", err)
	})
}

// rewritePluginStop corrupts the scaffold's plugin.sh so stop answers
// with destroy semantics (state deleted, nothing ongoing).
func rewritePluginStop(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "plugin.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, pair := range [][2]string{
		{`echo "stopped" > "$STATE_DIR/state-$ID"`, `echo "deleted" > "$STATE_DIR/state-$ID"`},
		{`emit stop "$ID" stopped`, `emit stop "$ID" deleted`},
		{`"ongoing":true,"note":"stopped node retains disks"`, `"ongoing":false,"note":"stopped node retains disks"`},
	} {
		if !strings.Contains(s, pair[0]) {
			t.Fatalf("plugin.sh stop branch piece not found: %s", pair[0])
		}
		s = strings.Replace(s, pair[0], pair[1], 1)
	}
	if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The executable scaffold passes validate and the fixture-only suite
// against its own plugin, with no source change.
func TestScaffoldExecutablePassesConformance(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.FormExecutable, Name: "scaffold-exec"}); err != nil {
		t.Fatalf("scaffold.Init failed: %v", err)
	}
	if err := scaffold.Validate(dir); err != nil {
		t.Fatalf("scaffold.Validate failed: %v", err)
	}
	conformance.Run(t, func(t *testing.T, fake *conformance.FakeAPI) conformance.Setup {
		return executableSetup(t, dir, fake)
	}, nil)
	liveDir := t.TempDir()
	if err := scaffold.Init(scaffold.Options{Dir: liveDir, Form: provider.FormExecutable, Name: "scaffold-exec-live"}); err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, func(t *testing.T, fake *conformance.FakeAPI) conformance.Setup {
		return executableSetup(t, liveDir, fake)
	}, nil, conformance.WithLive("scaffold-exec-live"))
}
