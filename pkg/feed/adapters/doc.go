// Package adapters provides reference channel adapters for the feed envelope
// contract (bashy-feed-envelope-v1).
//
// Each adapter is a thin translation layer between a provider's HTTP API and
// the symmetric Envelope shape in yoke/pkg/feed. Outbound: the adapter
// receives DeliveryRequest JSON on stdin (or via --message) and calls the
// provider. Inbound: it polls or receives webhooks and emits NDJSON envelopes
// on stdout for a supervised bashy service unit, or POSTs to the loopback
// endpoint.
//
// Reference code is adapted from OpenClaw (https://github.com/openclaw/openclaw,
// MIT, tag v2026.10.2). The Telegram and Matrix flows are converted from
// TypeScript to Go; Discord and Slack outbound are adapted from the same
// source. Every file carries an MIT attribution header naming the upstream
// commit. The adapters are wired ONLY through the Envelope contract — they
// hold no other coupling to the core.
package adapters
