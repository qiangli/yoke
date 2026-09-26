// Package cligw serves the OpenAI-compatible llmgw gateway from a pool of
// pre-spawned, one-shot agent CLI workers (claude, codex, agy, ...).
//
// A worker is started ahead of the request it will serve, receives exactly
// one prompt, is retired and replaced; no context crosses requests. Any agent
// in the fleet registry is a candidate; requests name a band (L4, L4+, auto),
// a registry model or an agent, and a policy picks the agent.
//
// NewServer wires the pieces into one http.Handler — FleetCatalog for the
// inventory, Pool + AgentBackend per agent, Router for the decision,
// Autoscaler for the spares — and NewCmd is the `llm serve|pools|env`
// command tree a host mounts (bashy llm).
package cligw
