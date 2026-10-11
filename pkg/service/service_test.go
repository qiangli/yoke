package service

import (
	"context"
	"errors"
	"testing"

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

func TestInternalProviderSwitchesCapabilityAfterRestart(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()
	oldCap, newCap := &fakeCap{}, &fakeCap{}
	if err := reg.Register("meet.old", oldCap); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("meet.new", newCap); err != nil {
		t.Fatal(err)
	}
	p := NewInternal("meet", reg)
	oldUnit := eventsUnit()
	oldUnit.Capability = "meet.old"
	if err := p.Activate(ctx, oldUnit, Runtime{}); err != nil {
		t.Fatal(err)
	}
	newUnit := oldUnit
	newUnit.Capability = "meet.new"
	if err := p.Configure(ctx, newUnit, Runtime{}); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("capability change: got %v, want restart", err)
	}
	if !oldCap.active || newCap.active {
		t.Fatal("configure changed the live capability")
	}
	if err := p.Deactivate(ctx, oldUnit.Name); err != nil || oldCap.active {
		t.Fatalf("old capability was not stopped: %v", err)
	}
	if err := p.Activate(ctx, newUnit, Runtime{}); err != nil {
		t.Fatal(err)
	}
	if !newCap.active || oldCap.active {
		t.Fatal("restart did not bind the new capability")
	}
	if err := p.Health(ctx, newUnit.Name); err != nil {
		t.Fatalf("health did not use new capability: %v", err)
	}
	if err := p.Deactivate(ctx, newUnit.Name); err != nil || newCap.active {
		t.Fatalf("new capability was not stopped: %v", err)
	}
}
