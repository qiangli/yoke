package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// fakeMonitor is the monitor end of the control socket: it answers `desired`
// from a settable set and records `report`s, using the same wire shape as
// bashy's supervisord.
type fakeMonitor struct {
	mu      sync.Mutex
	units   []Status
	reports []Status
	l       net.Listener
	path    string
}

func newFakeMonitor(t *testing.T) *fakeMonitor {
	t.Helper()
	dir, err := os.MkdirTemp("", "svc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m := &fakeMonitor{path: filepath.Join(dir, "s.sock")}
	m.l, err = net.Listen("unix", m.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.l.Close() })
	go func() {
		for {
			c, err := m.l.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	return m
}

func (m *fakeMonitor) serve(c net.Conn) {
	defer c.Close()
	var req ownerRequest
	if json.NewDecoder(bufio.NewReader(c)).Decode(&req) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := ownerResponse{OK: true}
	switch req.Op {
	case "desired":
		for _, u := range m.units {
			resp.Units = append(resp.Units, u)
		}
	case "report":
		m.reports = append(m.reports, req.Status)
	default:
		resp = ownerResponse{Error: "unknown op " + req.Op}
	}
	_ = json.NewEncoder(c).Encode(resp)
}

func (m *fakeMonitor) want(unit string, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.units = []Status{{Unit: unit, Provider: fleet.ServiceProviderInternal, Enabled: enabled, State: Starting}}
}

func (m *fakeMonitor) last() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reports) == 0 {
		return Status{}
	}
	return m.reports[len(m.reports)-1]
}

func newTestOwner(m *fakeMonitor, c Capability, decl *fleet.Service) *Owner {
	reg := NewRegistry()
	_ = reg.Register("meet.events", c)
	return &Owner{Name: "meet", Registry: reg, Client: Client{Socket: m.path},
		Resolve: func(string) (fleet.Service, error) { return *decl, nil }}
}

// The owner applies an enabled unit, reports it active, follows a disable,
// and applies a changed declaration live — through the generic provider.
func TestOwnerAppliesDesiredAndReports(t *testing.T) {
	m := newFakeMonitor(t)
	c := &fakeCap{}
	decl := eventsUnit()
	o := newTestOwner(m, c, &decl)
	ctx := context.Background()

	m.want("events", true)
	if err := o.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.active {
		t.Fatal("enabled unit was not activated")
	}
	if st := m.last(); st.Unit != "events" || st.State != Active || st.Health != "ok" || !st.Enabled {
		t.Fatalf("report = %+v", st)
	}

	decl.Settings = map[string]string{"retention": "30d"}
	if err := o.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c.cfg.Settings["retention"] != "30d" {
		t.Fatalf("changed declaration not applied live: %v", c.cfg.Settings)
	}

	m.want("events", false)
	if err := o.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c.active {
		t.Fatal("disabled unit still active")
	}
	if st := m.last(); st.State != Inactive || st.Enabled {
		t.Fatalf("report after disable = %+v", st)
	}
}

// A unit that was never enabled here still has its capability told it is
// off, once, so a capability that is live by default is withdrawn.
func TestOwnerSettlesDisabledUnitOnce(t *testing.T) {
	m := newFakeMonitor(t)
	c := &countingCap{}
	decl := eventsUnit()
	reg := NewRegistry()
	_ = reg.Register("meet.events", c)
	o := &Owner{Name: "meet", Registry: reg, Client: Client{Socket: m.path},
		Resolve: func(string) (fleet.Service, error) { return decl, nil }}
	m.want("events", false)
	for range 3 {
		if err := o.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.deactivations != 1 || c.activations != 0 {
		t.Fatalf("activations=%d deactivations=%d", c.activations, c.deactivations)
	}
	if st := m.last(); st.State != Inactive {
		t.Fatalf("report = %+v", st)
	}
}

type countingCap struct{ activations, deactivations int }

func (c *countingCap) Activate(context.Context, Config) error  { c.activations++; return nil }
func (c *countingCap) Configure(context.Context, Config) error { return nil }
func (c *countingCap) Deactivate(context.Context) error        { c.deactivations++; return nil }
func (c *countingCap) Health(context.Context) error            { return nil }

// An activation refused by the capability is reported failed with its reason.
func TestOwnerReportsFailedActivation(t *testing.T) {
	m := newFakeMonitor(t)
	decl := eventsUnit()
	o := newTestOwner(m, &fakeCap{failing: errors.New("no disk")}, &decl)
	m.want("events", true)
	if err := o.Sync(context.Background()); err == nil {
		t.Fatal("failed activation not surfaced")
	}
	if st := m.last(); st.State != Failed || st.Reason != "no disk" {
		t.Fatalf("report = %+v", st)
	}
}

// A restarted owner has no memory: the same desired set re-applies and
// re-reports with no extra handshake.
func TestOwnerReadoptsAfterRestart(t *testing.T) {
	m := newFakeMonitor(t)
	decl := eventsUnit()
	m.want("events", true)
	first := &fakeCap{}
	if err := newTestOwner(m, first, &decl).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := &fakeCap{}
	if err := newTestOwner(m, second, &decl).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !second.active || m.last().State != Active {
		t.Fatalf("re-adoption: active=%v report=%+v", second.active, m.last())
	}
}

func TestOwnerShutdownDeactivatesAndReports(t *testing.T) {
	m := newFakeMonitor(t)
	c := &fakeCap{}
	decl := eventsUnit()
	o := newTestOwner(m, c, &decl)
	m.want("events", true)
	if err := o.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.Shutdown()
	if c.active || m.last().State != Inactive {
		t.Fatalf("shutdown: active=%v report=%+v", c.active, m.last())
	}
}

func TestOwnerWithoutMonitorIsErrNoMonitor(t *testing.T) {
	decl := eventsUnit()
	o := &Owner{Name: "meet", Registry: NewRegistry(), Client: Client{Socket: filepath.Join(t.TempDir(), "none.sock")},
		Resolve: func(string) (fleet.Service, error) { return decl, nil }}
	if err := o.Sync(context.Background()); !errors.Is(err, ErrNoMonitor) {
		t.Fatalf("err = %v", err)
	}
}

func TestSupervisordSocketPath(t *testing.T) {
	t.Setenv("BASHY_SUPERVISORD_SOCKET", "")
	t.Setenv("BASHY_HOME", "/h")
	if got := SupervisordSocket(); got != filepath.Join("/h", "service", "supervisord.sock") {
		t.Fatal(got)
	}
	t.Setenv("BASHY_SUPERVISORD_SOCKET", "/s.sock")
	if got := SupervisordSocket(); got != "/s.sock" {
		t.Fatal(got)
	}
}
