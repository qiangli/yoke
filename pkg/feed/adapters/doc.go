// Package adapters provides reference channel adapters for the feed envelope
// contract (bashy-feed-envelope-v1).
//
// Each adapter is a thin translation layer between a provider's HTTP API and
// the symmetric Envelope shape in yoke/pkg/feed. Outbound: the adapter
// receives DeliveryRequest JSON on stdin (or via --message) and calls the
// provider. Inbound: Telegram and Matrix poll; Discord and Slack maintain
// vendor WebSocket connections and pass envelopes to the existing inbound
// handler and bridge. Those WebSocket paths require a pairing checker.
//
// Reference code is adapted from OpenClaw (https://github.com/openclaw/openclaw,
// MIT, tag v2026.10.2). The Telegram and Matrix flows are converted from
// TypeScript to Go; Discord and Slack outbound are adapted from the same
// source. The Gateway and Socket Mode inbound paths implement the vendors'
// published protocols. The adapters are wired through the Envelope contract.
package adapters
