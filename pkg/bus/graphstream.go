package bus

import (
	"io"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// subscriptionsSource is the stream source of `bus subscriptions --graph`.
const subscriptionsSource = "bus:subscriptions"

// writeSubscriptionsStream emits the standing subscriptions as a
// bashy-graph-v1 stream (Sprint 446 S19): who listens to what, and who may
// interrupt whom. Each subscriber is a node holding its interests (topic,
// kind, room, direct address) as edges; each named principal in
// InterruptFrom is a node with a "may-interrupt" edge to it. The sidecar's
// read offset (Since) and sink argv are left out: the offset moves on every
// poll, so it would change the rev without the subscription changing, and
// argv is an execution detail, not a relation.
func writeSubscriptionsStream(w io.Writer, subs []Subscription) error {
	b := stream.NewBuilder(subscriptionsSource)
	loc := func(inner ...string) string {
		return ref.Locator{Scheme: "bus", Target: "subscriptions", Inner: inner}.String()
	}
	b.Node(provider.Node{Ref: subscriptionsSource, Path: "subscriptions", Name: "subscriptions", Kind: "registry", Type: "bus/subscriptions"})
	interest := func(sub, kind, value, rel string) {
		r := loc(kind, value)
		b.Node(provider.Node{Ref: r, Path: kind, Name: value, Kind: kind, Type: "bus/" + kind})
		b.Edge(sub, rel, r, nil)
	}
	for _, s := range subs {
		r := loc("subscriber", s.Subscriber)
		attrs := map[string]any{}
		if s.Instance != "" {
			attrs["instance"] = s.Instance
		}
		if s.MaxPerMin > 0 {
			attrs["max_per_min"] = s.MaxPerMin
		}
		if s.Sink != "" {
			attrs["sink"] = s.Sink
		}
		if s.SinkTarget != "" {
			attrs["sink_target"] = s.SinkTarget
		}
		b.Node(provider.Node{Ref: r, Path: "subscriber", Name: s.Subscriber, Kind: "subscriber", Type: "bus/subscriber", Attrs: attrs})
		b.Edge(subscriptionsSource, "contains", r, nil)
		for _, t := range s.Topics {
			interest(r, "topic", t, "subscribes")
		}
		for _, k := range s.Kinds {
			interest(r, "kind", k, "filters")
		}
		if s.Room != "" {
			interest(r, "room", s.Room, "listens")
		}
		if s.To != "" {
			interest(r, "address", s.To, "receives")
		}
		for _, p := range s.InterruptFrom {
			pr := loc("principal", p)
			b.Node(provider.Node{Ref: pr, Path: "principal", Name: p, Kind: "principal", Type: "bus/principal"})
			b.Edge(pr, "may-interrupt", r, nil)
		}
	}
	return b.Encode(w, "bus", "op=subscriptions")
}
