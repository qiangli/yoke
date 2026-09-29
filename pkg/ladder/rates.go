package ladder

import "github.com/qiangli/yoke/pkg/ladder/blame"

// DeliveryRates reports whether a delivery event would move its agent's code
// rating in Replay: it names an agent and a story with valid points, and its
// outcome is 1 or 0.5, or 0 with a blame attribution that rates. Everything
// else is either unrated (counted) or skipped by Replay.
//
// It restates Replay's predicate for callers that need it without replaying;
// TestDeliveryRatesAgreesWithReplay pins the two together.
func DeliveryRates(e Event) bool {
	if e.Kind != EventKindDelivery || e.Agent == "" || e.Story == "" || !ValidPoints(e.Points) {
		return false
	}
	return e.Outcome == 1 || e.Outcome == .5 || (e.Outcome == 0 && blame.Rates(e.Blame))
}
