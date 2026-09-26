// Package llmgw is an OpenAI-compatible LLM gateway core: admission and
// fair-share scheduling, queueing, per-backend dispatch slots, circuit
// breaking, session affinity, model resolution, wire translation and
// tool-call extraction, behind a Backend interface.
//
// It is a nested module with no dependency on the rest of yoke (stdlib plus
// the Prometheus client only), so any server can embed it: bashy's local
// gateway over agent CLIs (pkg/cligw) and hosted pooled-model gateways alike.
//
// Subpackages:
//
//	openai   wire types, SSE, provider codecs, tool-call extraction, usage
//	sched    admit, queue, dispatch, breaker, affinity, history, job, metrics, limiter
//	resolve  classification and model resolution behind a Catalog
//	gateway  Backend interface and the stdlib http.Handler
package llmgw
