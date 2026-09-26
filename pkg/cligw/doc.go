// Package cligw serves the OpenAI-compatible llmgw gateway from a pool of
// pre-spawned, one-shot agent CLI workers (claude, codex, agy, ...).
//
// A worker is started ahead of the request it will serve, receives exactly
// one prompt, is retired and replaced; no context crosses requests. Any agent
// in the fleet registry is a candidate; requests name a band (L4, L4+, auto),
// a registry model or an agent, and a policy picks the agent.
package cligw

import (
	_ "github.com/qiangli/yoke/pkg/llmgw/gateway"
)
