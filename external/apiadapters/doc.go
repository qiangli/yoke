// Package apiadapters provides typed service API adapters for the
// high-value catalog gaps where native CLIs are absent or incomplete
// (Sprint 457 story cf3c288d595a):
//
//   - Slack data actions (history, channel list, message post/delete). The
//     slack CLI is app tooling (manifest, deploy, triggers, datastore) and
//     does not read or post messages, so messaging rides the Web API through
//     the already-merged external/saasclients adapter — reused here, never
//     duplicated.
//   - Notion page and database operations. No native CLI exists; the curl
//     candidate covers Notion as generic HTTP only.
//   - Airtable record operations. Same position: no native CLI, generic HTTP
//     only until this adapter.
//
// Everything is implemented from public API contracts (see EVIDENCE.md for
// the contract revisions); no n8n source is vendored, copied, or depended
// on. The catalog (research/n8n-cli/catalog.csv) was used only to SELECT
// the operations and to record the remaining families below.
//
// Shared machinery is reused, not redefined:
//   - HTTP transport, auth header, bounds, provider error classification:
//     saasclients.API (extended there with the Notion/Airtable error shapes).
//   - Explicit target + credential-file scope, mutation validation,
//     idempotency ledger: saasclients.Scope/Mutation/Ledger and the Ops
//     capability rows (extended there with the notion/airtable operations).
//   - Typed error codes, cursor binding validation, redaction, loom-v2
//     envelopes: external/gws. No new error ABI and no new envelope.
//
// This package is a library over explicit endpoints, not a registry
// external: it ships no binary, registers no command, and adds no atlas
// row (atlas external-count delta +0). BaseURL is always explicit — a
// production endpoint in use, a loopback fake in tests — never a default
// that could reach a real service by accident.
//
// Deliberately NOT here (recorded as unsupported or generic HTTP, never
// full n8n parity):
//   - Event triggers (Slack Events/Socket Mode, Notion polling triggers,
//     Airtable polling triggers): durable receivers need the events story
//     (5548854717bc) admission and are refused here.
//   - Notion search, users, comments, file uploads; Airtable batch writes,
//     attachments, views management, webhooks: generic HTTP via curl only.
//   - Every other catalog family keeps its owning 457 story (or generic
//     HTTP via curl where no story exists). See EVIDENCE.md.
package apiadapters

import (
	"strconv"

	"github.com/qiangli/yoke/external/gws"
)

// SchemaVersion is the version discriminator carried by every typed input
// and output in this package. Consumers advertise support and reject
// anything else.
const SchemaVersion = "apiadapters-v1"

// CheckVersion rejects an input carrying an unsupported schema version.
func CheckVersion(v string, op, target string) *gws.Error {
	if v == SchemaVersion {
		return nil
	}
	return gws.NewError(gws.CodeUnsupported,
		"apiadapters: "+op+" "+target+" needs schema_version "+SchemaVersion+", got "+quote(v), nil)
}

func quote(s string) string {
	if s == "" {
		return "(missing)"
	}
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return strconv.Quote(s)
}
