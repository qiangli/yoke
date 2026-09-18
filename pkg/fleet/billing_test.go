package fleet

import "testing"

// Billing is derived from Kind when absent, reproducing exactly what the collapsed enum
// used to mean — which is what makes this purely additive: no existing model needs an
// edit, and none changes behaviour.
func TestBillingDerivesFromKindWhenAbsent(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{ModelKindAPI, BillingMetered},
		{ModelKindLocal, BillingFree},
		// A vendor seat OVERRUNS into pay-as-you-go rather than blocking — Anthropic
		// Max/Pro and Codex all behave this way, and they are every subscription we have.
		{ModelKindSubscription, BillingFlatThenMetered},
	}
	for _, c := range cases {
		if got := (Model{Kind: c.kind}).BillingMode(); got != c.want {
			t.Errorf("kind %q derived billing %q, want %q", c.kind, got, c.want)
		}
	}
}

// An explicit billing always wins over the derivation — that is the entire point of the
// field. GLM: an API KEY (auth) on a FLAT PLAN (billing). No single enum value can say
// that, which is why there are two fields.
func TestExplicitBillingOverridesTheDerivation(t *testing.T) {
	glm := Model{Kind: ModelKindAPI, Billing: BillingFlat, CostMicro: 900}
	if got := glm.BillingMode(); got != BillingFlat {
		t.Fatalf("explicit billing %q was ignored; got %q", BillingFlat, got)
	}
	if glm.OverrunsIntoMoney() {
		t.Error("a HARD-quota flat plan must not report that it overruns into money — " +
			"it blocks, and conflating the two hides the one that costs real money")
	}
}

// THE DESIGN ERROR A TEST CAUGHT, pinned so it cannot come back.
//
// The first version priced every flat plan at a constant floor. That made a premium
// Opus/Codex SEAT marginally cheaper than metered DeepSeek — so the router would have
// sent every trivial task to the most expensive model in the fleet, inverting the whole
// point of the band ladder ("don't send a premium model to add a line of YAML").
//
// A flat plan is short of QUOTA, and quota scarcity SCALES WITH THE MODEL. A premium
// seat's quota is precious; a commodity seat's is not. So a flat plan is a discount on
// its OWN list price, never a flat floor.
func TestFlatPlansDoNotInvertTheFleetEconomy(t *testing.T) {
	premiumSeat := Model{Kind: ModelKindSubscription, CostMicro: 12_000} // codex:gpt-5.5
	commodityAPI := Model{Kind: ModelKindAPI, CostMicro: 1_500}          // deepseek-v4-pro

	if premiumSeat.MarginalCostMicro() <= commodityAPI.MarginalCostMicro() {
		t.Errorf("a premium SEAT (%d) is not dearer at the margin than a commodity metered model (%d) — "+
			"the router would send trivial work to the most expensive model in the fleet",
			premiumSeat.MarginalCostMicro(), commodityAPI.MarginalCostMicro())
	}
}

// ...and the other half of the truth, which must hold at the same time: a flat plan
// still beats a METERED PEER OF ITS OWN CLASS. Capacity you have already paid for is
// wasted by not using it.
func TestAFlatPlanBeatsAMeteredPeerOfTheSameClass(t *testing.T) {
	glmFlat := Model{Kind: ModelKindAPI, Billing: BillingFlat, CostMicro: 900}
	deepseekMetered := Model{Kind: ModelKindAPI, CostMicro: 1_500}

	if glmFlat.MarginalCostMicro() >= deepseekMetered.MarginalCostMicro() {
		t.Errorf("a flat-rate plan (%d) is not cheaper at the margin than a metered peer (%d) — "+
			"the seat is already bought; not using it is a waste, not a saving",
			glmFlat.MarginalCostMicro(), deepseekMetered.MarginalCostMicro())
	}
}

// The distinction that actually bites an unattended run.
//
//	flat              -> quota gone: the agent STOPS. A reliability event. Loud.
//	flat_then_metered -> quota gone: the agent KEEPS GOING AND BILLS YOU. Silent.
//
// A fleet run that exhausts a subscription seat does not fail — it moves onto
// pay-as-you-go, and you find out on the invoice.
func TestOverrunsIntoMoneyIsTheOneAnUnattendedRunMustKnow(t *testing.T) {
	if !(Model{Kind: ModelKindSubscription}).OverrunsIntoMoney() {
		t.Error("a vendor seat must report that it overruns into billing — that is how " +
			"Anthropic Max/Pro and Codex behave, and an unattended run needs to know")
	}
	if (Model{Kind: ModelKindAPI, Billing: BillingFlat}).OverrunsIntoMoney() {
		t.Error("a HARD-quota flat plan reported an overrun into money it cannot make")
	}
	if (Model{Kind: ModelKindLocal}).OverrunsIntoMoney() {
		t.Error("local inference cannot bill you")
	}
	if (Model{Kind: ModelKindAPI}).OverrunsIntoMoney() {
		t.Error("a metered model has no quota to overrun — it bills from the first token")
	}
}

// Below quota, the two flat modes price identically. They differ in the FAILURE MODE,
// not the price, and pricing them apart would be inventing a distinction the money does
// not make.
func TestBothFlatModesPriceTheSameAtTheMargin(t *testing.T) {
	hard := Model{Kind: ModelKindAPI, Billing: BillingFlat, CostMicro: 4_000}
	overrun := Model{Kind: ModelKindAPI, Billing: BillingFlatThenMetered, CostMicro: 4_000}
	if hard.MarginalCostMicro() != overrun.MarginalCostMicro() {
		t.Errorf("flat (%d) and flat_then_metered (%d) priced differently below quota — "+
			"the seat is bought either way; they differ in what happens when it runs out",
			hard.MarginalCostMicro(), overrun.MarginalCostMicro())
	}
}
