// Package service is the runtime contract every service unit is realized
// through (docs/bashy-service-design.md, Sprint 418 S15/S16/S18).
//
// The declaration is fleet.Service — data in the registry rings. This package
// is the other half: the states a unit moves through, the event a transition
// emits, and the Provider interface the monitor drives. There are exactly two
// providers and the unit's `provider:` field picks one; nothing here, nor in
// the monitor or the exposure reconciler, branches on a unit's NAME.
//
//   - process  (S16) owns a foreground child: exec, process group, restart.
//   - internal (this package; wired by S18) applies the declaration to a
//     Capability its owning subsystem registered in-process. There is no
//     child and no pid — Status.PID stays 0 — and no feature flag beside it:
//     a capability is on exactly when its unit is enabled and activated.
//
// The init bootstrap (outpost supervisord starting the outpost daemon and
// bashy supervisord) is deliberately NOT a provider: init supervises the
// monitor, the monitor supervises units.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// State is a unit's runtime state. The set is closed; consumers (status
// JSON, exposure, notify) match on these values, never on text.
type State string

const (
	Inactive State = "inactive" // not running; the desired state may be either
	Starting State = "starting" // activated, health not yet established
	Active   State = "active"   // running and healthy (or no probe declared)
	Degraded State = "degraded" // running, health failing below the restart threshold
	Failed   State = "failed"   // start limit hit, a dependency failed, or activation refused
	Stopping State = "stopping" // deactivation in progress
)

// Exposed reports whether a unit in this state may be published on the
// tunnel and mesh. Everything else is withdrawn.
func (s State) Exposed() bool { return s == Active || s == Degraded }

// Status is one unit's structured state — what `bashy service status --json`
// prints, the admin UI and MCP show, and outpost's exposure reconciler reads.
type Status struct {
	Unit     string    `json:"unit"`
	Provider string    `json:"provider"`
	Enabled  bool      `json:"enabled"` // desired state, from the declaration
	State    State     `json:"state"`
	Since    time.Time `json:"since,omitzero"`
	PID      int       `json:"pid,omitempty"` // process provider only
	Restarts int       `json:"restarts,omitempty"`
	LastExit *int      `json:"last_exit,omitempty"`
	Health   string    `json:"health,omitempty"` // last probe verdict
	Reason   string    `json:"reason,omitempty"` // why failed / degraded / inactive
	Ports    []string  `json:"ports,omitempty"`
	Exposed  []string  `json:"exposed,omitempty"` // app/mesh names currently published
}

// Event is a state transition. The monitor appends it to the room timeline
// as kind service.<To> (S3), and Failed reaches the notify sinks (S7).
type Event struct {
	Unit   string    `json:"unit"`
	From   State     `json:"from"`
	To     State     `json:"to"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Kind is the timeline kind the event is published under.
func (e Event) Kind() string { return "service." + string(e.To) }

// Runtime is what a provider receives besides the declaration: values the
// monitor resolves once so no provider re-implements them.
type Runtime struct {
	// Env is the unit's environment with secrets already resolved.
	Env map[string]string
	// Download is the provisioned path of the unit's pinned release, if any.
	Download string
	// Emit reports a transition the provider observed on its own (a child
	// exiting, a capability failing). Never nil when the monitor calls.
	Emit func(Event)
}

// Provider realizes units of one provider kind. The monitor calls it; it
// never decides policy (restart, start limits, ordering, exposure) — those
// are the monitor's, read from the declaration, identical for both kinds.
type Provider interface {
	// Name is the declaration's provider value this realizes.
	Name() string
	// Activate brings the unit up. It returns once the unit is Starting.
	Activate(ctx context.Context, unit fleet.Service, rt Runtime) error
	// Configure applies a changed declaration to a running unit. A provider
	// that cannot apply it live returns ErrRestartRequired and the monitor
	// restarts the unit.
	Configure(ctx context.Context, unit fleet.Service, rt Runtime) error
	// Deactivate takes the unit down; idempotent.
	Deactivate(ctx context.Context, unit string) error
	// Health runs the unit's own check. The declared http/tcp/exec probe is
	// the monitor's; Health is the provider-specific liveness under it
	// (child alive, capability serving).
	Health(ctx context.Context, unit string) error
	// Status is the provider's view; the monitor overlays policy fields.
	Status(unit string) Status
}

var (
	// ErrRestartRequired: the change cannot be applied without a restart.
	ErrRestartRequired = errors.New("service: change requires a restart")
	// ErrNotOwner: an internal unit belongs to another process.
	ErrNotOwner = errors.New("service: capability is owned by another process")
	// ErrNoProvider: the declaration names a provider that is not installed.
	ErrNoProvider = errors.New("service: no provider for unit")
)

// Providers maps provider names to implementations.
type Providers map[string]Provider

// For returns the provider realizing unit — chosen by the declaration's
// provider field and nothing else.
func (p Providers) For(unit fleet.Service) (Provider, error) {
	if prov, ok := p[unit.ProviderName()]; ok {
		return prov, nil
	}
	return nil, errors.Join(ErrNoProvider, errors.New(unit.Name+": provider "+unit.ProviderName()))
}
