// Package event is the dependency-light publishing core of pkg/bus.
package event

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

const (
	DeliveryQueued    = "queued"
	DeliveryInterrupt = "interrupt"

	// TopicSpacetimeMovement announces a network-coordinate transition.
	TopicSpacetimeMovement = "spacetime.movement"
)

// Notification is one attributed and addressed change to announce.
type Notification struct {
	Principal   string
	Topic       string
	To          string
	Room        string
	Body        string
	Priority    string
	Activity    *room.Activity
	MatchReason string
}

// Publish is the common enforcement path for CLI and programmatic publishers.
func Publish(n Notification) error {
	if n.Principal == "" {
		return fmt.Errorf("bus: principal is required (REPORT/AUTHOR invariant)")
	}
	if n.Topic == "" && n.To == "" && n.Room == "" {
		return fmt.Errorf("bus: addressing is required — set at least one of Topic, To, or Room (an unaddressed notification reaches nobody)")
	}
	switch n.Priority {
	case "", DeliveryQueued, DeliveryInterrupt:
	default:
		return fmt.Errorf("bus: unknown priority %q (use %s or %s)", n.Priority, DeliveryQueued, DeliveryInterrupt)
	}
	if n.Activity != nil {
		if err := n.Activity.Validate(); err != nil {
			return err
		}
		if n.Activity.Actor != n.Principal {
			return fmt.Errorf("bus: activity actor must match principal")
		}
	}
	return room.Notify(room.Event{
		Principal:   n.Principal,
		Topic:       n.Topic,
		To:          n.To,
		Room:        n.Room,
		Body:        n.Body,
		Priority:    n.Priority,
		Activity:    n.Activity,
		MatchReason: n.MatchReason,
	})
}

// PublishSpacetimeMovement announces only coordinate names, never values or
// the private network signals used to derive place identity.
func PublishSpacetimeMovement(changed []string) error {
	if len(changed) == 0 {
		return nil
	}
	for _, coordinate := range changed {
		if coordinate != "place.id" && coordinate != "net.same_lan" {
			return fmt.Errorf("bus: unsupported movement coordinate %q", coordinate)
		}
	}
	return Publish(Notification{
		Principal: "spacetime",
		Topic:     TopicSpacetimeMovement,
		Body:      "network coordinate changed: " + strings.Join(changed, ", "),
		Priority:  DeliveryQueued,
	})
}
