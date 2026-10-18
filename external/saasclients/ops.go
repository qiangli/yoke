package saasclients

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"

	"github.com/qiangli/yoke/external/gws"
)

// Via says how an operation reaches the provider.
type Via string

const (
	ViaCLI     Via = "cli"     // the admitted CLI documents the operation
	ViaAPI     Via = "api"     // the CLI does not expose it: documented REST/Web API adapter (api.go)
	ViaRefused Via = "refused" // neither admitted
)

// Effect classifies what an operation does.
type Effect string

const (
	EffectRead      Effect = "read"
	EffectWrite     Effect = "write"      // reversible data mutation inside the target
	EffectDelete    Effect = "delete"     // destructive
	EffectRealWorld Effect = "real-world" // sends a message or moves money: sandbox/test target only
)

// Idem is the idempotency strategy for a mutation.
type Idem string

const (
	IdemNone      Idem = ""             // read: not applicable
	IdemProvider  Idem = "provider-key" // provider dedupes on a client key (Stripe Idempotency-Key)
	IdemNaturalID Idem = "natural-id"   // upsert on a caller-supplied external id (Salesforce)
	IdemLedger    Idem = "ledger"       // provider has no key: the Ledger records the intent and refuses a replay
)

// Op is one capability row. Rows are the per-client product/API scope
// verification: a development CLI is NOT assumed to expose every CRUD verb.
type Op struct {
	Client string
	Name   string
	Via    Via
	Effect Effect
	Idem   Idem
	Note   string
}

// Ops is the reviewed capability table. Mapping is from vendor reference
// documentation; no row was executed against a live account (EVIDENCE.md).
var Ops = []Op{
	// Salesforce — sf covers query and single-record CRUD; upsert by external
	// id is only in the bulk CSV path, so the single-record form needs REST.
	{"sf", "query", ViaCLI, EffectRead, IdemNone, "sf data query (SOQL)"},
	{"sf", "record-get", ViaCLI, EffectRead, IdemNone, "sf data get record"},
	{"sf", "record-create", ViaCLI, EffectWrite, IdemLedger, "sf data create record; no provider key"},
	{"sf", "record-update", ViaCLI, EffectWrite, IdemNaturalID, "sf data update record by id: re-applying the same values converges"},
	{"sf", "record-delete", ViaCLI, EffectDelete, IdemNaturalID, "sf data delete record by id: second delete is a typed not-found"},
	{"sf", "record-upsert", ViaAPI, EffectWrite, IdemNaturalID, "REST PATCH /sobjects/{Obj}/{ExtIdField}/{value}; sf only upserts via bulk CSV"},
	// Stripe — stripe-cli's resource commands are generated from the API and
	// carry --idempotency.
	{"stripe", "read", ViaCLI, EffectRead, IdemNone, "stripe <resource> retrieve|list"},
	{"stripe", "create", ViaCLI, EffectWrite, IdemProvider, "stripe <resource> create --idempotency KEY"},
	{"stripe", "update", ViaCLI, EffectWrite, IdemProvider, "stripe <resource> update --idempotency KEY"},
	{"stripe", "delete", ViaCLI, EffectDelete, IdemProvider, "stripe <resource> delete"},
	{"stripe", "payment-confirm", ViaCLI, EffectRealWorld, IdemProvider, "payment_intents confirm|capture, charges, refunds, payouts: test-mode key only"},
	// Twilio — twilio-cli exposes the REST API as api:core:… commands; none is idempotent.
	{"twilio", "read", ViaCLI, EffectRead, IdemNone, "twilio api:core:<resource>:fetch|list"},
	{"twilio", "create", ViaCLI, EffectWrite, IdemLedger, "twilio api:core:<resource>:create (non-message); no provider key"},
	{"twilio", "update", ViaCLI, EffectWrite, IdemNaturalID, "twilio api:core:<resource>:update by SID"},
	{"twilio", "delete", ViaCLI, EffectDelete, IdemNaturalID, "twilio api:core:<resource>:remove by SID"},
	{"twilio", "message-send", ViaCLI, EffectRealWorld, IdemLedger, "api:core:messages:create / calls:create: sandbox account and Twilio test credentials only"},
	// Slack — slack-cli is an app-development CLI (manifest, deploy, triggers,
	// datastore); it does not read or post messages. Messaging is Web API.
	{"slack", "app-manifest", ViaCLI, EffectRead, IdemNone, "slack manifest"},
	{"slack", "datastore-get", ViaCLI, EffectRead, IdemNone, "slack datastore get|query"},
	{"slack", "datastore-put", ViaCLI, EffectWrite, IdemNaturalID, "slack datastore put (keyed by primary key)"},
	{"slack", "datastore-delete", ViaCLI, EffectDelete, IdemNaturalID, "slack datastore delete"},
	{"slack", "history", ViaAPI, EffectRead, IdemNone, "Web API conversations.history"},
	{"slack", "channel-list", ViaAPI, EffectRead, IdemNone, "Web API conversations.list"},
	{"slack", "message-post", ViaAPI, EffectRealWorld, IdemLedger, "Web API chat.postMessage: sandbox workspace only; no provider key"},
	{"slack", "message-delete", ViaAPI, EffectDelete, IdemNaturalID, "Web API chat.delete (ts)"},
	// Notion — no native CLI exists (the curl candidate covers it as generic
	// HTTP only), so every operation below uses the documented REST adapter in
	// external/apiadapters (Sprint 457 story cf3c288d595a).
	{"notion", "page-get", ViaAPI, EffectRead, IdemNone, "GET /v1/pages/{page_id}"},
	{"notion", "database-query", ViaAPI, EffectRead, IdemNone, "POST /v1/databases/{database_id}/query"},
	{"notion", "page-create", ViaAPI, EffectWrite, IdemLedger, "POST /v1/pages; no provider key"},
	{"notion", "page-update", ViaAPI, EffectWrite, IdemNaturalID, "PATCH /v1/pages/{page_id}: re-applying the same properties converges"},
	{"notion", "page-archive", ViaAPI, EffectDelete, IdemNaturalID, "PATCH /v1/pages/{page_id} archived=true"},
	// Airtable — same position: no native CLI, generic HTTP only until the
	// documented REST adapter in external/apiadapters.
	{"airtable", "record-list", ViaAPI, EffectRead, IdemNone, "GET /v0/{base}/{table}"},
	{"airtable", "record-get", ViaAPI, EffectRead, IdemNone, "GET /v0/{base}/{table}/{record_id}"},
	{"airtable", "record-create", ViaAPI, EffectWrite, IdemLedger, "POST /v0/{base}/{table}; no provider key"},
	{"airtable", "record-update", ViaAPI, EffectWrite, IdemNaturalID, "PATCH /v0/{base}/{table}/{record_id}"},
	{"airtable", "record-delete", ViaAPI, EffectDelete, IdemNaturalID, "DELETE /v0/{base}/{table}/{record_id}"},
}

// LookupOp finds a capability row.
func LookupOp(client, name string) (Op, bool) {
	for _, o := range Ops {
		if o.Client == client && o.Name == name {
			return o, true
		}
	}
	return Op{}, false
}

// OpNames lists a client's operation names, sorted.
func OpNames(client string) []string {
	var out []string
	for _, o := range Ops {
		if o.Client == client {
			out = append(out, o.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Mutation is a validated-before-effects intent.
type Mutation struct {
	Scope   Scope
	Op      string
	Key     string // caller idempotency key; derived from payload when empty
	Payload []byte
	DryRun  bool
}

// DeriveKey is the deterministic idempotency key for (scope, op, payload).
func DeriveKey(scope Scope, op string, payload []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", scope.Client, scope.Target, op)
	h.Write(payload)
	return "bashy-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// Plan is the validated outcome of Mutation.Validate.
type Plan struct {
	Op   Op
	Key  string
	Idem Idem
	Via  Via
}

// Validate checks everything that can be checked before any effect: the op is
// admitted, a real-world op targets a declared sandbox, the payload is
// bounded, and the idempotency key exists. A dry run still validates.
func (m Mutation) Validate() (Plan, *gws.Error) {
	op, ok := LookupOp(m.Scope.Client, m.Op)
	target := m.Scope.Client + ":" + m.Op
	if !ok {
		return Plan{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: operation %q is not admitted (known: %v)", m.Scope.Client, m.Op, OpNames(m.Scope.Client)), nil).WithOp("validate", target)
	}
	if op.Effect == EffectRead {
		return Plan{}, gws.NewError(gws.CodeInvalidInput, target+" is a read, not a mutation", nil).WithOp("validate", target)
	}
	if len(m.Payload) > gws.MaxPayloadBytes {
		return Plan{}, gws.NewError(gws.CodeOutputLimit, fmt.Sprintf("%s: payload %d bytes exceeds %d", target, len(m.Payload), gws.MaxPayloadBytes), nil).WithOp("validate", target)
	}
	if op.Effect == EffectRealWorld && !m.Scope.Sandbox {
		return Plan{}, gws.NewError(gws.CodePermissionDenied, fmt.Sprintf("%s has real-world effect (message/payment) and the target %q is not a declared sandbox (%s=1)", target, m.Scope.Target, EnvName(m.Scope.Client, "SANDBOX")), nil).WithOp("validate", target)
	}
	key := m.Key
	if key == "" {
		key = DeriveKey(m.Scope, m.Op, m.Payload)
	}
	return Plan{Op: op, Key: key, Idem: op.Idem, Via: op.Via}, nil
}

// Ledger records applied mutation keys so a replay is detected and refused
// (IdemLedger) rather than repeated. It is in-memory: a durable ledger across
// processes is a named gap.
type Ledger struct {
	mu   sync.Mutex
	done map[string]string
}

// Begin claims a key. A key already claimed is a replay: the caller must not
// repeat the mutation; the prior outcome is returned.
func (l *Ledger) Begin(key string) (prior string, replay bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done == nil {
		l.done = map[string]string{}
	}
	if p, ok := l.done[key]; ok {
		return p, true
	}
	l.done[key] = "unknown"
	return "", false
}

// Finish records the outcome (confirmed|failed|unknown) for a claimed key.
// A failed outcome releases the key so a retry is permitted; unknown does not.
func (l *Ledger) Finish(key, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if outcome == "failed" {
		delete(l.done, key)
		return
	}
	l.done[key] = outcome
}
