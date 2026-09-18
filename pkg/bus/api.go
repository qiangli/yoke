package bus

import (
	busevent "github.com/qiangli/yoke/pkg/bus/event"
)

// TopicSpacetimeMovement announces that the host's network coordinate changed.
// The notification deliberately names only the changed coordinate, never the
// raw network observations from which place identity was derived.
const TopicSpacetimeMovement = busevent.TopicSpacetimeMovement

// Notification is one change to announce.
type Notification = busevent.Notification

// Publish announces a change on the bus.
//
// This is the one enforcement point, and it exists because the invariants used to
// live only inside the cobra handler: anything publishing programmatically —
// the coach, kb, a future conductor — would have bypassed the principal and
// addressing checks entirely and written a notification that could reach nobody.
// A rule enforced in the CLI layer is a rule that holds only for humans.
func Publish(n Notification) error {
	return busevent.Publish(n)
}

// PublishSpacetimeMovement announces a debounced network-coordinate change.
// changed must contain only coordinate names; values are intentionally absent
// so gateway, SSID, DNS and peer details cannot enter the append-only timeline.
func PublishSpacetimeMovement(changed []string) error {
	return busevent.PublishSpacetimeMovement(changed)
}
