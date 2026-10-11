package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Capability is an in-process feature its owning subsystem registers under
// an id (outpost.files, meet.events, …). The subsystem keeps its own
// implementation; the declaration only decides whether, and how configured,
// it runs. Config carries everything the unit declares that a capability can
// consume.
type Capability interface {
	Activate(ctx context.Context, cfg Config) error
	Configure(ctx context.Context, cfg Config) error
	Deactivate(ctx context.Context) error
	Health(ctx context.Context) error
}

// Config is the declaration as an internal capability sees it.
type Config struct {
	Unit     string
	Settings map[string]string
	Env      map[string]string
	Ports    []fleet.ServicePort
}

// ConfigOf projects a unit and its resolved runtime into a Config.
func ConfigOf(unit fleet.Service, rt Runtime) Config {
	return Config{Unit: unit.Name, Settings: unit.Settings, Env: rt.Env, Ports: unit.Ports}
}

// Registry holds the capabilities this process hosts.
type Registry struct {
	mu   sync.RWMutex
	caps map[string]Capability
}

// NewRegistry returns an empty capability registry.
func NewRegistry() *Registry { return &Registry{caps: map[string]Capability{}} }

// Register adds a capability; an id may be registered once.
func (r *Registry) Register(id string, c Capability) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.caps[id]; dup {
		return fmt.Errorf("service: capability %q registered twice", id)
	}
	r.caps[id] = c
	return nil
}

// IDs lists registered capability ids, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.caps))
	for id := range r.caps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) get(id string) (Capability, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.caps[id]
	return c, ok
}

// Internal is the internal provider for one hosting process. self is that
// process's owner name: "outpost" for the outpost daemon, or the name of the
// unit whose process hosts capabilities (meet hosts meet.events).
type Internal struct {
	self string
	reg  *Registry
	now  func() time.Time

	mu    sync.Mutex
	units map[string]*internalUnit
}

type internalUnit struct {
	capID   string
	status  Status
	settled bool // the capability was told it is off while the unit never ran
}

// NewInternal returns the internal provider for the process named self.
func NewInternal(self string, reg *Registry) *Internal {
	return &Internal{self: self, reg: reg, now: time.Now, units: map[string]*internalUnit{}}
}

// Name implements Provider.
func (p *Internal) Name() string { return fleet.ServiceProviderInternal }

func (p *Internal) capability(unit fleet.Service) (Capability, error) {
	if unit.Owner != p.self {
		return nil, fmt.Errorf("%w: %s is hosted by %s, not %s", ErrNotOwner, unit.Name, unit.Owner, p.self)
	}
	c, ok := p.reg.get(unit.Capability)
	if !ok {
		return nil, fmt.Errorf("service: %s: capability %q is not registered in %s", unit.Name, unit.Capability, p.self)
	}
	return c, nil
}

func (p *Internal) transition(unit string, to State, reason string, rt Runtime) {
	p.mu.Lock()
	u := p.units[unit]
	from := u.status.State
	u.status.State, u.status.Reason, u.status.Since = to, reason, p.now()
	p.mu.Unlock()
	if rt.Emit != nil && from != to {
		rt.Emit(Event{Unit: unit, From: from, To: to, Reason: reason, At: p.now()})
	}
}

// Activate implements Provider.
func (p *Internal) Activate(ctx context.Context, unit fleet.Service, rt Runtime) error {
	c, err := p.capability(unit)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if existing, ok := p.units[unit.Name]; ok {
		if existing.status.State != Inactive && existing.capID != unit.Capability {
			p.mu.Unlock()
			return ErrRestartRequired
		}
		existing.capID, existing.settled = unit.Capability, false
		existing.status.Enabled = unit.Enabled
	} else {
		p.units[unit.Name] = &internalUnit{capID: unit.Capability,
			status: Status{Unit: unit.Name, Provider: p.Name(), Enabled: unit.Enabled, State: Inactive}}
	}
	p.mu.Unlock()
	p.transition(unit.Name, Starting, "", rt)
	if err := c.Activate(ctx, ConfigOf(unit, rt)); err != nil {
		p.transition(unit.Name, Failed, err.Error(), rt)
		return err
	}
	p.transition(unit.Name, Active, "", rt)
	return nil
}

// Configure implements Provider. Changing the implementation or owner needs a
// stop/start cycle so the old capability is deactivated before the new one runs.
func (p *Internal) Configure(ctx context.Context, unit fleet.Service, rt Runtime) error {
	p.mu.Lock()
	old, exists := p.units[unit.Name]
	changed := exists && (old.capID != unit.Capability || unit.Owner != p.self)
	p.mu.Unlock()
	if changed {
		return ErrRestartRequired
	}
	c, err := p.capability(unit)
	if err != nil {
		return err
	}
	return c.Configure(ctx, ConfigOf(unit, rt))
}

// Deactivate implements Provider.
func (p *Internal) Deactivate(ctx context.Context, unit string) error {
	p.mu.Lock()
	u, ok := p.units[unit]
	state := Inactive
	capID := ""
	if ok {
		state, capID = u.status.State, u.capID
	}
	p.mu.Unlock()
	if !ok || state == Inactive {
		return nil
	}
	c, _ := p.reg.get(capID)
	p.transition(unit, Stopping, "", Runtime{})
	if c != nil {
		if err := c.Deactivate(ctx); err != nil {
			p.transition(unit, Failed, err.Error(), Runtime{})
			return err
		}
	}
	p.transition(unit, Inactive, "", Runtime{})
	return nil
}

// Settle makes sure the capability behind a unit that is not wanted is off,
// even if this process never activated it (a capability that is live by
// default until its owner is told otherwise). It tells the capability once;
// a unit that is running is deactivated normally. Capability.Deactivate must
// be idempotent.
func (p *Internal) Settle(ctx context.Context, unit fleet.Service) error {
	p.mu.Lock()
	u, ok := p.units[unit.Name]
	if ok && u.status.State != Inactive {
		p.mu.Unlock()
		return p.Deactivate(ctx, unit.Name)
	}
	if ok && u.settled && u.capID == unit.Capability {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	c, err := p.capability(unit)
	if err != nil {
		return err
	}
	if err := c.Deactivate(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.units[unit.Name]; ok {
		u.capID, u.settled = unit.Capability, true
		u.status.Enabled = unit.Enabled
	} else {
		p.units[unit.Name] = &internalUnit{capID: unit.Capability, settled: true,
			status: Status{Unit: unit.Name, Provider: p.Name(), Enabled: unit.Enabled, State: Inactive, Since: p.now()}}
	}
	return nil
}

// Health implements Provider.
func (p *Internal) Health(ctx context.Context, unit string) error {
	p.mu.Lock()
	u, ok := p.units[unit]
	capID := ""
	if ok {
		capID = u.capID
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("service: %s is not active here", unit)
	}
	c, ok := p.reg.get(capID)
	if !ok {
		return fmt.Errorf("service: %s: capability %q gone", unit, capID)
	}
	return c.Health(ctx)
}

// Status implements Provider. PID is always 0: an internal unit has none.
func (p *Internal) Status(unit string) Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.units[unit]; ok {
		return u.status
	}
	return Status{Unit: unit, Provider: p.Name(), State: Inactive}
}
