package herald

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// Card is what a peer says about itself.
//
// Everything here is a CLAIM. skills[] in particular is self-asserted prose
// with no schema — A2A has no inputSchema — so it is recorded as a prior and
// never as a capability measurement.
type Card struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	URL         string   `json:"url,omitempty"`
	Streaming   bool     `json:"streaming"`
	Skills      []Skill  `json:"skills,omitempty"`
	Extensions  []string `json:"extensions,omitempty"`
}

// Skill is one advertised capability.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// SupportsGate reports whether the peer declares herald's evidence extension.
// When false, the gate still runs — locally. See gate.go.
func (c Card) SupportsGate() bool {
	for _, u := range c.Extensions {
		if u == GateExtensionURI {
			return true
		}
	}
	return false
}

// Discover fetches and validates a peer's AgentCard.
//
// The card is fetched from CardPath — /.well-known/agent-card.json. Reaching
// for the pre-v0.2 /.well-known/agent.json is the most common way to end up
// talking v0.3 by accident.
func Discover(ctx context.Context, baseURL string) (Card, error) {
	raw, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		return Card{}, fmt.Errorf("herald: resolving agent card at %s%s: %w", strings.TrimSuffix(baseURL, "/"), CardPath, err)
	}
	return cardFrom(raw), nil
}

// cardFrom projects the SDK's card onto ours, so the SDK's types stop at this
// file. a2a-go is a young dependency; nothing downstream should have to know
// it exists.
func cardFrom(raw *a2a.AgentCard) Card {
	if raw == nil {
		return Card{}
	}
	c := Card{
		Name:        raw.Name,
		Description: raw.Description,
		Version:     raw.Version,
	}
	c.Streaming = raw.Capabilities.Streaming
	for _, e := range raw.Capabilities.Extensions {
		c.Extensions = append(c.Extensions, e.URI)
	}
	for _, s := range raw.Skills {
		c.Skills = append(c.Skills, Skill{
			ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags,
		})
	}
	for _, i := range raw.SupportedInterfaces {
		if i != nil && i.URL != "" {
			c.URL = i.URL
			break
		}
	}
	return c
}

// Result is the outcome of delegating one task to a peer.
type Result struct {
	SchemaVersion string        `json:"schema_version"`
	Peer          string        `json:"peer"`
	TaskID        string        `json:"task_id,omitempty"`
	State         string        `json:"state"`
	Text          string        `json:"text,omitempty"`
	Artifacts     []string      `json:"artifacts,omitempty"`
	Gate          GateOutcome   `json:"gate"`
	Elapsed       time.Duration `json:"elapsed_ns,omitempty"`
}

// Succeeded is the ONLY success predicate callers may use.
//
// When a gate was configured, the gate decides. Without one, the peer's
// completed state is accepted for command composition but remains explicitly
// unverified in Gate.
func (r Result) Succeeded() bool {
	if r.Gate.Ran {
		return r.Gate.Passed
	}
	return strings.EqualFold(r.State, "completed")
}

// ExitCode maps a result onto a process exit status, so a peer composes with
// `&&` like any other command.
func (r Result) ExitCode() int {
	switch {
	case r.Gate.Ran && r.Gate.Passed:
		return 0
	case r.Gate.Ran:
		return 1
	case strings.EqualFold(r.State, "completed"):
		return 0 // successful peer completion; Gate still records UNVERIFIED
	default:
		return 1
	}
}

// SendOptions configures one delegation.
type SendOptions struct {
	// Gate is the command that decides whether the peer's work is good.
	Gate string
	// GateDir is where a local gate runs. Defaults to the process cwd.
	GateDir string
	// Stream requests incremental updates. Requires the peer to advertise
	// streaming; herald falls back to a blocking send when it does not.
	Stream bool
	// OnText receives streamed text as it arrives, for line-buffered stdout.
	OnText func(string)
}

// Send delegates a task to a peer and returns a gated result.
func Send(ctx context.Context, p Peer, prompt string, opts SendOptions) (Result, error) {
	start := time.Now()
	res := Result{SchemaVersion: SchemaVersion, Peer: p.Name}

	card, err := Discover(ctx, p.URL)
	if err != nil {
		return res, err
	}

	client, err := newClient(ctx, p, card)
	if err != nil {
		return res, err
	}
	defer client.Destroy()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt))
	req := &a2a.SendMessageRequest{Message: msg}

	var sb strings.Builder
	if opts.Stream && card.Streaming {
		for ev, err := range client.SendStreamingMessage(ctx, req) {
			if err != nil {
				return res, fmt.Errorf("herald: streaming from %s: %w", p.Name, err)
			}
			absorb(ev, &res, &sb, opts.OnText)
		}
	} else {
		out, err := client.SendMessage(ctx, req)
		if err != nil {
			return res, fmt.Errorf("herald: sending to %s: %w", p.Name, err)
		}
		absorbResult(out, &res, &sb, opts.OnText)
	}
	res.Text = sb.String()
	res.Elapsed = time.Since(start)

	// The verdict. A peer that declares the extension is expected to return
	// its own evidence; until that half is negotiated on the wire we re-run
	// the gate here regardless, because a peer's evidence about itself is
	// still the peer talking about itself.
	res.Gate = RunLocalGate(ctx, opts.GateDir, opts.Gate, res.State)
	return res, nil
}

// newClient builds an A2A client for a peer, wiring auth from the vault.
func newClient(ctx context.Context, p Peer, card Card) (*a2aclient.Client, error) {
	raw, err := agentcard.DefaultResolver.Resolve(ctx, p.URL)
	if err != nil {
		return nil, fmt.Errorf("herald: %w", err)
	}
	c, err := a2aclient.NewFromCard(ctx, raw, protocolOptions()...)
	if err != nil {
		return nil, fmt.Errorf("herald: client for %s (herald speaks A2A %s): %w", p.Name, ProtocolVersion, err)
	}
	return c, nil
}

// protocolOptions pins the client to the A2A version herald declares.
//
// This is the single call site of ProtocolVersion, and it is what makes the
// constant load-bearing rather than decorative. The mechanics are indirect
// enough to be worth spelling out:
//
// a2a-go has no "set the version" knob. The A2A-Version service parameter it
// puts on every request — a2aclient.Client sends
// serviceParams[a2a.SvcParamVersion] on each call, and the JSON-RPC/REST
// transports turn service params into HTTP headers — is taken from the version
// of the TRANSPORT it negotiated. Transports are registered in a factory keyed
// by (protocol, version), and the SDK's own defaults (WithJSONRPCTransport /
// WithRESTTransport) register at the SDK's a2a.Version. So calling
// NewFromCard with no options delegates the wire version to whatever constant
// the compiled-in dependency happens to carry.
//
// Registering the same two transports at ProtocolVersion instead has three
// consequences, all of them the point:
//
//   - the A2A-Version herald sends is herald's, stated here;
//   - a card advertising only a major version herald does not speak — v0.3,
//     whose wire format is incompatible — matches no transport and fails
//     loudly at construction, rather than being silently negotiated;
//   - an SDK release that moves a2a.Version cannot change our wire behind our
//     back; the version test fails first.
//
// Defaults are disabled so the SDK's a2a.Version registrations do not sit
// alongside ours: an extra registration at a different version would give
// selectTransport a second candidate to pick.
func protocolOptions() []a2aclient.FactoryOption {
	v := a2a.ProtocolVersion(ProtocolVersion)
	return []a2aclient.FactoryOption{
		a2aclient.WithDefaultsDisabled(),
		// Order mirrors the SDK's defaults: JSON-RPC first, then REST.
		a2aclient.WithCompatTransport(v, a2a.TransportProtocolJSONRPC,
			a2aclient.TransportFactoryFn(func(ctx context.Context, card *a2a.AgentCard, iface *a2a.AgentInterface) (a2aclient.Transport, error) {
				return a2aclient.NewJSONRPCTransport(iface.URL, nil), nil
			})),
		a2aclient.WithCompatTransport(v, a2a.TransportProtocolHTTPJSON,
			a2aclient.TransportFactoryFn(func(ctx context.Context, card *a2a.AgentCard, iface *a2a.AgentInterface) (a2aclient.Transport, error) {
				u, err := url.Parse(iface.URL)
				if err != nil {
					return nil, fmt.Errorf("herald: peer endpoint %q is not a URL: %w", iface.URL, err)
				}
				return a2aclient.NewRESTTransport(u, nil), nil
			})),
	}
}

// Credential resolves a peer's API key WITHOUT ever returning it to a caller
// that only needs to know whether one exists.
//
// The value comes from the process environment, which is how the bashy vault
// delivers secrets (`eval "$(bashy secret env)"`). herald therefore never
// reads, caches, or logs the secret store itself.
func Credential(p Peer) (string, bool) {
	if p.APIKeyRef == "" {
		return "", false
	}
	v, ok := os.LookupEnv(p.APIKeyRef)
	return v, ok && v != ""
}

// absorb folds one streamed event into the result.
func absorb(ev a2a.Event, res *Result, sb *strings.Builder, onText func(string)) {
	switch e := ev.(type) {
	case *a2a.Task:
		res.TaskID = string(e.ID)
		res.State = stateName(e.Status.State)
	case *a2a.TaskStatusUpdateEvent:
		res.TaskID = string(e.TaskID)
		res.State = stateName(e.Status.State)
	case *a2a.TaskArtifactUpdateEvent:
		for _, part := range e.Artifact.Parts {
			if t := partText(part); t != "" {
				sb.WriteString(t)
				if onText != nil {
					onText(t)
				}
			}
		}
		if e.Artifact.Name != "" {
			res.Artifacts = append(res.Artifacts, e.Artifact.Name)
		}
	case *a2a.Message:
		for _, part := range e.Parts {
			if t := partText(part); t != "" {
				sb.WriteString(t)
				if onText != nil {
					onText(t)
				}
			}
		}
	}
}

// absorbResult folds a blocking send's result into the result.
func absorbResult(out a2a.SendMessageResult, res *Result, sb *strings.Builder, onText func(string)) {
	switch v := out.(type) {
	case *a2a.Task:
		res.TaskID = string(v.ID)
		res.State = stateName(v.Status.State)
		for _, art := range v.Artifacts {
			for _, part := range art.Parts {
				if t := partText(part); t != "" {
					sb.WriteString(t)
					if onText != nil {
						onText(t)
					}
				}
			}
		}
	case *a2a.Message:
		for _, part := range v.Parts {
			if t := partText(part); t != "" {
				sb.WriteString(t)
				if onText != nil {
					onText(t)
				}
			}
		}
		res.State = "completed"
	}
}

// partText extracts text from a Part. A2A v1.0 discriminates parts by JSON
// member presence, not by a "kind" field — text is simply the member that is
// set.
func partText(p *a2a.Part) string {
	if p == nil {
		return ""
	}
	return p.Text()
}

// stateName renders a task state without the TASK_STATE_ prefix.
func stateName(s a2a.TaskState) string {
	return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
}
