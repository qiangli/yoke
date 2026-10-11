package meet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/qiangli/yoke/pkg/service"
)

// EventsCapabilityID is the capability the `events` unit declares.
const EventsCapabilityID = "meet.events"

// eventsCapability is the host event log feed (GET /api/events). Until a
// monitor has told this process otherwise it serves, so a hand-run
// `meet serve` keeps the feature; once a monitor manages the unit, the unit's
// enabled state is the only switch.
type eventsCapability struct{ state atomic.Int32 } // 0 unmanaged, 1 on, 2 off

var meetEvents = &eventsCapability{}

func (c *eventsCapability) serving() bool { return c.state.Load() != 2 }

func (c *eventsCapability) Activate(context.Context, service.Config) error {
	c.state.Store(1)
	return nil
}
func (c *eventsCapability) Configure(context.Context, service.Config) error { return nil }
func (c *eventsCapability) Deactivate(context.Context) error {
	c.state.Store(2)
	return nil
}
func (c *eventsCapability) Health(context.Context) error {
	if !c.serving() {
		return fmt.Errorf("meet: %s is off", EventsCapabilityID)
	}
	return nil
}

// eventsGate answers 404 while the events unit is off.
func eventsGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !meetEvents.serving() {
			writeErr(w, http.StatusNotFound, fmt.Errorf("meet: the events unit is not enabled (bashy service enable events)"))
			return
		}
		h(w, r)
	}
}

// newServiceOwner is the service.Owner for this meet process: it applies the
// internal units the monitor says meet hosts and reports their state.
func newServiceOwner(log io.Writer) (*service.Owner, error) {
	reg := service.NewRegistry()
	if err := reg.Register(EventsCapabilityID, meetEvents); err != nil {
		return nil, err
	}
	return &service.Owner{Name: "meet", Registry: reg, Log: log}, nil
}
