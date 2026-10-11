package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

type fakeCap struct {
	active  bool
	cfg     Config
	failing error
}

func (f *fakeCap) Activate(_ context.Context, c Config) error {
	if f.failing != nil {
		return f.failing
	}
	f.active, f.cfg = true, c
	return nil
}
func (f *fakeCap) Configure(_ context.Context, c Config) error { f.cfg = c; return nil }
func (f *fakeCap) Deactivate(context.Context) error            { f.active = false; return nil }
func (f *fakeCap) Health(context.Context) error {
	if !f.active {
		return errors.New("down")
	}
	return nil
}

func eventsUnit() fleet.Service {
	return fleet.Service{Name: "events", Kind: fleet.KindService, Provider: fleet.ServiceProviderInternal,
		Capability: "meet.events", Owner: "meet", Enabled: true, Settings: map[string]string{"retention": "7d"}}
}

// The provider is selected by the declaration's provider field, never by
// the unit's name: a unit renamed or freshly declared routes identically.
func TestProvidersDispatchOnProviderField(t *testing.T) {
	in := NewInternal("meet", NewRegistry())
	ps := Providers{fleet.ServiceProviderInternal: in}
	for _, name := range []string{"events", "anything-else"} {
		u := eventsUnit()
		u.Name = name
		if p, err := ps.For(u); err != nil || p != in {
			t.Fatalf("%s: provider = %v, %v", name, p, err)
		}
	}
	if _, err := ps.For(fleet.Service{Name: "meet", Exec: []string{"x"}}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("process unit without a process provider: %v", err)
	}
}

func TestInternalProviderLifecycleHasNoPID(t *testing.T) {
	reg := NewRegistry()
	c := &fakeCap{}
	if err := reg.Register("meet.events", c); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("meet.events", c); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	p := NewInternal("meet", reg)
	var events []Event
	rt := Runtime{Env: map[string]string{"A": "1"}, Emit: func(e Event) { events = append(events, e) }}
	ctx := context.Background()
	if err := p.Activate(ctx, eventsUnit(), rt); err != nil {
		t.Fatal(err)
	}
	st := p.Status("events")
	if st.State != Active || st.PID != 0 || !c.active || c.cfg.Settings["retention"] != "7d" || c.cfg.Env["A"] != "1" {
		t.Fatalf("after activate: status=%+v cap=%+v", st, c)
	}
	if len(events) != 2 || events[0].To != Starting || events[1].Kind() != "service.active" {
		t.Fatalf("events = %+v", events)
	}
	u := eventsUnit()
	u.Settings = map[string]string{"retention": "1d"}
	if err := p.Configure(ctx, u, rt); err != nil || c.cfg.Settings["retention"] != "1d" {
		t.Fatalf("live configure: %v %+v", err, c.cfg)
	}
	if err := p.Health(ctx, "events"); err != nil {
		t.Fatal(err)
	}
	if err := p.Deactivate(ctx, "events"); err != nil || c.active || p.Status("events").State != Inactive {
		t.Fatalf("deactivate: %v active=%v", err, c.active)
	}
}

func TestInternalProviderRefusesForeignOwnerAndReportsFailure(t *testing.T) {
	reg := NewRegistry()
	c := &fakeCap{failing: errors.New("listener busy")}
	_ = reg.Register("meet.events", c)
	if err := NewInternal("outpost", reg).Activate(context.Background(), eventsUnit(), Runtime{}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign owner: %v", err)
	}
	p := NewInternal("meet", reg)
	if err := p.Activate(context.Background(), eventsUnit(), Runtime{}); err == nil {
		t.Fatal("failing capability activated")
	}
	if st := p.Status("events"); st.State != Failed || st.Reason != "listener busy" || st.State.Exposed() {
		t.Fatalf("status = %+v", st)
	}
}

type fakeProcUnit struct {
	unit    fleet.Service
	rt      Runtime
	state   State
	pid     int
	healthy bool
	argv    []string
}

type fakeProcessProvider struct {
	mu      sync.Mutex
	units   map[string]*fakeProcUnit
	nextPID int
}

func newFakeProcessProvider() *fakeProcessProvider {
	return &fakeProcessProvider{
		units:   make(map[string]*fakeProcUnit),
		nextPID: 1000,
	}
}

func (p *fakeProcessProvider) Name() string { return fleet.ServiceProviderProcess }

func (p *fakeProcessProvider) Activate(ctx context.Context, unit fleet.Service, rt Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	argv, err := unit.ExpandExec(rt.Download)
	if err != nil {
		return err
	}
	p.nextPID++
	u := &fakeProcUnit{
		unit:    unit,
		rt:      rt,
		state:   Starting,
		pid:     p.nextPID,
		healthy: true,
		argv:    argv,
	}
	p.units[unit.Name] = u
	if rt.Emit != nil {
		rt.Emit(Event{Unit: unit.Name, From: Inactive, To: Starting, At: time.Now()})
		u.state = Active
		rt.Emit(Event{Unit: unit.Name, From: Starting, To: Active, At: time.Now()})
	} else {
		u.state = Active
	}
	return nil
}

func (p *fakeProcessProvider) Configure(ctx context.Context, unit fleet.Service, rt Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.units[unit.Name]
	if !ok || u.state != Active {
		return errors.New("unit not active")
	}
	newArgv, err := unit.ExpandExec(rt.Download)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(u.argv, newArgv) {
		return ErrRestartRequired
	}
	u.unit = unit
	u.rt = rt
	return nil
}

func (p *fakeProcessProvider) Deactivate(ctx context.Context, unit string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.units[unit]
	if !ok {
		return nil
	}
	if u.rt.Emit != nil && u.state != Inactive {
		u.rt.Emit(Event{Unit: unit, From: u.state, To: Inactive, At: time.Now()})
	}
	u.state = Inactive
	u.pid = 0
	return nil
}

func (p *fakeProcessProvider) Health(ctx context.Context, unit string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.units[unit]
	if !ok || u.state != Active || !u.healthy {
		return errors.New("unhealthy")
	}
	return nil
}

func (p *fakeProcessProvider) Status(unit string) Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.units[unit]
	if !ok {
		return Status{Unit: unit, Provider: fleet.ServiceProviderProcess, State: Inactive}
	}
	return Status{
		Unit:     unit,
		Provider: fleet.ServiceProviderProcess,
		Enabled:  u.unit.Enabled,
		State:    u.state,
		PID:      u.pid,
	}
}

func fakeExternalUnit(name string, port int, healthPath string) fleet.Service {
	return fleet.Service{
		Name:     name,
		Kind:     fleet.KindService,
		Provider: fleet.ServiceProviderProcess,
		Exec:     []string{"bashy", name, "serve", "--port", "{port}", "--addr", "{bind}", "--data={settings.data_dir}"},
		Settings: map[string]string{"data_dir": ""},
		Ports:    []fleet.ServicePort{{Name: "http", Port: port}},
		Health:   fleet.ServiceHealth{HTTP: healthPath},
		Enabled:  true,
	}
}

func TestProcessProviderDispatchAndManagerRuntime(t *testing.T) {
	proc := newFakeProcessProvider()
	in := NewInternal("meet", NewRegistry())
	ps := Providers{
		fleet.ServiceProviderProcess:  proc,
		fleet.ServiceProviderInternal: in,
	}

	services := []struct {
		name string
		port int
		path string
	}{
		{"zot", 5000, "/v2/"},
		{"seaweedfs", 8333, "/"},
		{"kopia", 51515, "/"},
	}

	for _, tc := range services {
		u := fakeExternalUnit(tc.name, tc.port, tc.path)

		// 1. Dispatch selects process provider
		p, err := ps.For(u)
		if err != nil || p != proc {
			t.Fatalf("%s: expected process provider, got %v (%v)", tc.name, p, err)
		}

		// 2. Lifecycle activation
		var events []Event
		rt := Runtime{
			Env:  map[string]string{"ENV_TEST": "1"},
			Emit: func(e Event) { events = append(events, e) },
		}
		ctx := context.Background()
		if err := p.Activate(ctx, u, rt); err != nil {
			t.Fatalf("%s activate: %v", tc.name, err)
		}

		st := p.Status(tc.name)
		if st.State != Active || st.PID <= 0 || !st.State.Exposed() {
			t.Fatalf("%s status after activate: %+v", tc.name, st)
		}
		if len(events) != 2 || events[0].To != Starting || events[1].To != Active {
			t.Fatalf("%s events = %+v", tc.name, events)
		}

		// 3. Health check
		if err := p.Health(ctx, tc.name); err != nil {
			t.Fatalf("%s health: %v", tc.name, err)
		}

		// 4. Configure with same argv succeeds
		if err := p.Configure(ctx, u, rt); err != nil {
			t.Fatalf("%s configure unchanged: %v", tc.name, err)
		}

		// 5. Configure with changed port requires restart
		uChanged := u
		uChanged.Ports = []fleet.ServicePort{{Name: "http", Port: tc.port + 1}}
		if err := p.Configure(ctx, uChanged, rt); !errors.Is(err, ErrRestartRequired) {
			t.Fatalf("%s configure changed port: want ErrRestartRequired, got %v", tc.name, err)
		}

		// 6. Deactivate takes unit down
		if err := p.Deactivate(ctx, tc.name); err != nil {
			t.Fatalf("%s deactivate: %v", tc.name, err)
		}
		st = p.Status(tc.name)
		if st.State != Inactive || st.PID != 0 || st.State.Exposed() {
			t.Fatalf("%s status after deactivate: %+v", tc.name, st)
		}
	}
}

func TestManagerRuntime_LocalFakeServiceLiveHTTPHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// Parse host and port from server URL
	srvURL := server.URL // e.g. http://127.0.0.1:xxxxx
	parts := strings.Split(strings.TrimPrefix(srvURL, "http://"), ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected server url: %s", srvURL)
	}
	var port int
	fmt.Sscanf(parts[1], "%d", &port)

	proc := newFakeProcessProvider()
	ctx := context.Background()

	for _, name := range []string{"zot", "seaweedfs", "kopia"} {
		healthPath := "/"
		if name == "zot" {
			healthPath = "/v2/"
		}
		u := fakeExternalUnit(name, port, healthPath)
		rt := Runtime{
			Emit: func(e Event) {},
		}
		if err := proc.Activate(ctx, u, rt); err != nil {
			t.Fatalf("activate %s: %v", name, err)
		}

		// Probe the local fake server endpoint
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath))
		if err != nil {
			t.Fatalf("%s live health probe: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status code = %d, want 200", name, resp.StatusCode)
		}

		if err := proc.Deactivate(ctx, name); err != nil {
			t.Fatalf("deactivate %s: %v", name, err)
		}
	}
}
