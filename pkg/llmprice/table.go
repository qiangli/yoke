package llmprice

// seedModels is the built-in price table: USD per million tokens.
//
// PROVENANCE RULE: every number here is carried from the seed table this
// package was extracted from (ycode internal/runtime/usage/pricing.go), which
// recorded each provider's public list price, or is the same weights reached
// through another spelling (a Bedrock route, a vendor path prefix). No number
// is invented, guessed or converted from the fleet catalog's cost_micro —
// cost_micro is a relative routing weight, not a price, and turning it into
// dollars would manufacture a fact. A model with no documented per-million
// price is simply absent, which reads as unpriced: no price is better than a
// wrong one (the same rule pkg/fleet/baseline/models/bedrock-glm-5.yaml states
// for the metered Bedrock route).
//
// Subscription seats (claude-opus5.5, codex-gpt6-sol, muse-spark1.3, ...) have
// a marginal cost of zero that is a property of the PLAN, not of the model.
// They are deliberately absent: pricing them here would bill a prepaid seat as
// if it were metered. Local engine models (ollama) are absent for the same
// reason — the door records them as unpriced, not as $0, so a report can tell
// "free" from "we do not know".
//
// A row must name the model it prices. "glm" is not a row: a bare family alias
// would price every future GLM at today's number, which is exactly the
// confident-wrong-number failure this package exists to end. Add the model.
func seedModels() map[string]Pricing {
	m := map[string]Pricing{}

	// set prices one id. Point releases are listed explicitly, at their
	// family's documented rate, because this table matches no prefix: a dated
	// id is a nameable model, not a wildcard.
	set := func(id string, in, out, cacheWrite, cacheRead float64) {
		m[NormalizeModel(id)] = Pricing{
			InputPerM: in, OutputPerM: out,
			CacheWritePerM: cacheWrite, CacheReadPerM: cacheRead,
			Known: true,
		}
	}

	const (
		// Anthropic Claude 4.x / 3.5, API list prices.
		opusIn, opusOut, opusCW, opusCR         = 15.0, 75.0, 18.75, 1.50
		sonnetIn, sonnetOut, sonnetCW, sonnetCR = 3.0, 15.0, 3.75, 0.30
		haikuIn, haikuOut, haikuCW, haikuCR     = 0.80, 4.0, 1.0, 0.08
		// z.ai GLM and Moonshot Kimi, list API prices.
		glmIn, glmOut, glmCR    = 0.60, 2.20, 0.11
		kimiIn, kimiOut, kimiCR = 0.60, 2.50, 0.15
		// DeepSeek (cache read = the cache-hit input price).
		dsFlashIn, dsFlashOut, dsFlashCR = 0.14, 0.28, 0.0028
		dsProIn, dsProOut, dsProCR       = 0.435, 0.87, 0.003625
	)

	// ---- Anthropic Claude --------------------------------------------------
	set("claude-opus-4", opusIn, opusOut, opusCW, opusCR)
	set("claude-opus-4-8", opusIn, opusOut, opusCW, opusCR)
	set("claude-sonnet-4", sonnetIn, sonnetOut, sonnetCW, sonnetCR)
	set("claude-sonnet-4-6", sonnetIn, sonnetOut, sonnetCW, sonnetCR)
	set("claude-haiku-4", haikuIn, haikuOut, haikuCW, haikuCR)
	set("claude-haiku-4-5", haikuIn, haikuOut, haikuCW, haikuCR)
	set("claude-3-5-sonnet", sonnetIn, sonnetOut, sonnetCW, sonnetCR)
	set("claude-3-5-haiku", haikuIn, haikuOut, haikuCW, haikuCR)

	// ---- OpenAI ------------------------------------------------------------
	set("gpt-4o", 2.50, 10.0, 0, 1.25)
	set("gpt-4o-mini", 0.15, 0.60, 0, 0.075)
	set("gpt-4.1", 2.0, 8.0, 0, 0.50)
	set("gpt-4.1-mini", 0.40, 1.60, 0, 0.10)
	set("gpt-4.1-nano", 0.10, 0.40, 0, 0.025)
	set("o3", 10.0, 40.0, 0, 5.0)
	set("o3-mini", 1.10, 4.40, 0, 0.55)
	set("o4-mini", 1.10, 4.40, 0, 0.55)

	// ---- Google Gemini -----------------------------------------------------
	set("gemini-2.5-pro", 1.25, 10.0, 0, 0.315)
	set("gemini-2.5-flash", 0.15, 0.60, 0, 0.0375)

	// ---- DeepSeek ----------------------------------------------------------
	set("deepseek-chat", dsFlashIn, dsFlashOut, 0, dsFlashCR)
	set("deepseek-reasoner", dsFlashIn, dsFlashOut, 0, dsFlashCR)
	set("deepseek-v4-flash", dsFlashIn, dsFlashOut, 0, dsFlashCR)
	set("deepseek-v4-pro", dsProIn, dsProOut, 0, dsProCR)

	// ---- z.ai GLM ----------------------------------------------------------
	// The z.ai LIST rate for the GLM coding models (the seed table's number).
	// The fleet's glm-5.3 rows ride a prepaid Coding Plan seat whose marginal
	// cost is zero, which is a property of the plan, not of the model — the
	// list rate is what the same weights cost metered, and it is the only
	// price a per-token table can honestly state.
	//
	// glm-5.3-flash is deliberately ABSENT: it is a flash tier with no rate in
	// the seed table, and a cheaper sibling is not its parent's rate.
	set("glm-4.5", glmIn, glmOut, 0, glmCR)
	set("glm-4.6", glmIn, glmOut, 0, glmCR)
	set("glm-4.7", glmIn, glmOut, 0, glmCR)
	set("glm-5", glmIn, glmOut, 0, glmCR)
	set("glm-5.1", glmIn, glmOut, 0, glmCR)
	set("glm-5.2", glmIn, glmOut, 0, glmCR)
	set("glm-5.3", glmIn, glmOut, 0, glmCR)

	// ---- Moonshot / Kimi ---------------------------------------------------
	// kimi-k2.7-code's LIST rate. The highspeed serving tier (a higher price
	// by Moonshot's own account, ratio unread) and kimi-k3 (no rate read) are
	// deliberately ABSENT rather than priced at k2.7's number.
	set("kimi-k2.7-code", kimiIn, kimiOut, 0, kimiCR)
	set("moonshot/kimi-k2.7-code", kimiIn, kimiOut, 0, kimiCR)

	// ---- Alibaba Qwen / xAI Grok -------------------------------------------
	set("qwen-max", 0.40, 1.20, 0, 0)
	set("grok-3", 3.00, 15.00, 0, 0)

	// ---- AWS Bedrock -------------------------------------------------------
	// The ids Bedrock actually answers to, in the spellings this repository's
	// fleet seeds pin. A Bedrock row is priced at the rate of the weights it
	// serves — the route changes the endpoint, not the price — and only when
	// those weights have a documented rate above:
	//
	//   us.anthropic.claude-opus-4-8  models/bedrock-opus4.8.yaml, the UAT
	//                                  route, priced as claude-opus-4-8.
	//   zai.glm-5                     models/bedrock-glm-5.yaml, priced as
	//                                  the GLM list rate.
	//   openai.gpt-6-sol              models/bedrock-gpt6-sol.yaml — NO ROW
	//                                  ON PURPOSE. No per-million price for
	//                                  gpt-6-sol is documented anywhere in
	//                                  this repository (its native route is a
	//                                  subscription seat, and cost_micro is a
	//                                  relative weight). Until one is, the
	//                                  door records it as unpriced.
	set("us.anthropic.claude-opus-4-8", opusIn, opusOut, opusCW, opusCR)
	set("eu.anthropic.claude-opus-4-8", opusIn, opusOut, opusCW, opusCR)
	set("zai.glm-5", glmIn, glmOut, 0, glmCR)

	return m
}
