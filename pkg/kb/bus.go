package kb

// The bus bridge: a SUPERSEDE invalidates a fact, and whoever is relying on that
// fact needs to hear about it.
//
// kb is the durable PULL half of the coordination substrate — it answers "what is
// true about this host" when an agent goes looking. That works for every kb
// operation except one. An ADD or an UPDATE is fine to discover later: nobody is
// acting on a page that does not exist yet, and an improved page is still the same
// page. A SUPERSEDE is different in kind, because it says something an agent may
// ALREADY HAVE READ AND BELIEVED is wrong. Waiting for that agent to go looking
// again is waiting for it to re-derive a conclusion it thinks it has settled.
//
// So supersede — and only supersede — also pushes.

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/bus"
)

// supersedeTopic is the per-page topic a reader subscribes to.
//
// Hierarchical, so a subscriber can take one page (`kb.page.the-slug`), a
// prefix, or everything (`kb.page.*`) — the bus matches whole dotted segments,
// and a slug is already the flat, dashed identity kb enforces.
func supersedeTopic(slug string) string { return "kb.page." + slug }

// publishSuperseded announces that a page was replaced.
//
// QUEUED, not interrupt, and that is a deliberate line. The bus's biggest failure
// mode is pollution, and the rule is that only direction-changers push. A
// superseded fact is a correction to shared knowledge, relevant to whoever next
// acts on it — not a reason to break into every subscriber's running turn for
// what is, most of the time, a wiki edit. The coach's `fleet.unresolved` earns the
// interrupt tier because it names one specific agent that is stuck right now;
// this is a broadcast, and a broadcast that interrupts is how a bus becomes noise.
//
// An operator who knows a particular correction IS urgent can still publish one
// by hand at the interrupt tier.
//
// Best-effort: a bus that cannot be written must never fail a kb write. The page
// is already corrected on disk; losing the announcement degrades this to the pull
// behaviour kb has always had, which is the status quo rather than a new failure.
// A nil replacement means the page was invalidated with nothing to replace it —
// `kb update --status superseded`, the second route into this state. Announcing
// on only one of the two routes would be the same shape of bug the coach's
// supervisor alert had: a condition that is mode-independent, reported from only
// one of the paths that produce it.
func publishSuperseded(old, replacement *Page) {
	if old == nil {
		return
	}
	body := fmt.Sprintf("kb page %q was SUPERSEDED — if you relied on it, stop and re-check before acting", old.Slug)
	if replacement != nil {
		body = fmt.Sprintf("kb page %q was SUPERSEDED by %q (%s) — if you relied on it, re-read before acting: bashy kb show %s",
			old.Slug, replacement.Slug, replacement.Title, replacement.Slug)
	}
	_ = bus.Publish(bus.Notification{
		Principal: ToolID(),
		Topic:     supersedeTopic(old.Slug),
		Body:      body,
		Priority:  bus.DeliveryQueued,
	})
}
