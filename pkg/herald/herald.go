// Package herald reaches agents that are not on this host.
//
// Every other bashy coordination verb — meet, weave, foreman, delegate —
// addresses a participant as a tool:model binding that resolves to a BINARY
// ON THIS HOST. herald is the one path to a capability that is not installed
// here and cannot be: an agent on someone else's infrastructure, reached over
// the Agent2Agent (A2A) protocol.
//
// # A peer is an ordinary tool:model binding
//
// herald deliberately introduces no new registry. A peer is a fleet Model
// carrying the peer's URL, and `herald` is the tool that dials it:
//
//	# models/acme-reviewer.yaml
//	name:        acme-reviewer
//	kind:        api
//	provider:    a2a                        # the discriminator
//	base_url:    https://agent.example.com
//	api_key_ref: ACME_TOKEN                 # resolved through `bashy secret`
//	band:        0                          # unpegged until host-measured
//
// so the agent `herald:acme-reviewer` lands in the capability matrix, in
// `meet --participant`, in weave and foreman, with no special case anywhere.
// That is what makes an external agent first-class: not new orchestration
// code, but no new code at all.
//
// The alternative — a new fleet Tool kind — was rejected: Tool has nowhere to
// put a URL (it is entirely process-launch argv), IsCLI() is a hard equality
// test gating four call sites, and the cloudbox sync drops non-CLI tools
// outright, so such an entry could never reach the org catalog ring.
//
// # A2A v1.0, not v0.3
//
// The wire here is A2A v1.0, which is NOT compatible with the v0.3 that most
// documentation still describes. The differences that bite:
//
//   - the card lives at /.well-known/agent-card.json, not agent.json
//   - methods are PascalCase (SendMessage, GetTask), not message/send
//   - A2A-Version must be sent explicitly; absent, a server assumes 0.3
//   - Part is a JSON-member oneof ({"text":…}/{"url":…}/{"data":…}) with no
//     "kind" discriminator
//
// # Evidence, not assertion
//
// A2A's terminal TASK_STATE_COMPLETED is self-reported by the peer. herald
// accepts that completion by default and labels it unverified. Callers that
// need an authoritative verdict opt into a gate — see gate.go. A peer that
// ignores the gate extension is measured locally when a gate was supplied.
package herald

import (
	"fmt"
	"strings"
)

// SchemaVersion is the envelope version for herald's --json output.
const SchemaVersion = "bashy-herald-v1"

// ProviderA2A marks a fleet Model as an A2A peer rather than an LLM. It rides
// the existing free-form `provider:` field precisely so no schema changes and
// no new asset kind are needed.
const ProviderA2A = "a2a"

// ToolName is the fleet tool that dials a peer. An external agent is the
// binding ToolName + ":" + <peer name>.
const ToolName = "herald"

// CardPath is the A2A v1.0 well-known location of an agent card. Registered
// with IANA per the spec; the pre-v0.2 spelling was /.well-known/agent.json
// and reaching for it is the single most common way to talk to the wrong
// protocol version.
const CardPath = "/.well-known/agent-card.json"

// ProtocolVersion is the A2A version herald speaks. It MUST be sent on every
// request: a server that receives no A2A-Version assumes "0.3", whose wire
// format is incompatible with this one.
//
// It is not a label. protocolOptions in client.go registers herald's A2A
// transports at this version, which is what the SDK derives the outgoing
// A2A-Version header from — so changing this constant changes the wire, and a
// peer that speaks only an incompatible major version fails at client
// construction instead of being silently negotiated down.
// TestA2AVersionOnTheWire pins the header a peer actually receives.
const ProtocolVersion = "1.0"

// Peer is an external agent herald can reach: the projection of a fleet Model
// whose provider is ProviderA2A.
type Peer struct {
	// Name is the local handle. `herald:<Name>` is the agent binding.
	Name string `json:"name" yaml:"name"`
	// URL is the peer's base URL — the card is fetched from URL+CardPath.
	URL string `json:"url" yaml:"base_url"`
	// APIKeyRef names the secret in the bashy vault, never the value.
	APIKeyRef string `json:"api_key_ref,omitempty" yaml:"api_key_ref,omitempty"`
	// Display is a human label; falls back to Name.
	Display string `json:"display,omitempty" yaml:"display,omitempty"`
	// Band is the capability peg. ALWAYS 0 for a freshly added peer: an
	// AgentCard's skills[] is a self-asserted claim, and a claim is a prior.
	// Only a host-measured gate produces a band.
	Band int `json:"band" yaml:"band,omitempty"`
}

// Binding returns the tool:model form under which the peer is dispatched.
func (p Peer) Binding() string { return ToolName + ":" + p.Name }

// CardURL is where the peer's AgentCard should be served.
func (p Peer) CardURL() string {
	return strings.TrimSuffix(p.URL, "/") + CardPath
}

// Validate rejects a peer that could not be dialled.
func (p Peer) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("herald: peer needs a name")
	}
	if strings.ContainsAny(p.Name, " \t/:") {
		return fmt.Errorf("herald: peer name %q may not contain spaces, %q or %q", p.Name, "/", ":")
	}
	if p.URL == "" {
		return fmt.Errorf("herald: peer %q needs a base URL", p.Name)
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return fmt.Errorf("herald: peer %q URL must be http(s), got %q", p.Name, p.URL)
	}
	return nil
}
