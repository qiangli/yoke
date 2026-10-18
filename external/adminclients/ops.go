package adminclients

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"

	"github.com/qiangli/yoke/external/gws"
)

// Effect classifies what an operation does.
type Effect string

const (
	EffectRead   Effect = "read"
	EffectWrite  Effect = "write"  // reversible directory mutation
	EffectDelete Effect = "delete" // destructive; the Reverse of a create
)

// Op is one capability row. Rows are mapped from vendor reference
// documentation; none was executed against a live tenant (EVIDENCE.md).
type Op struct {
	Client string
	Name   string
	Effect Effect
	// Scope is the single least-privilege permission the op needs.
	Scope string
	// GWSGap names the gws Deferred key this GAM op fills ("" for m365).
	GWSGap string
	// Reverse is the op that undoes this one (cleanup path), "" for reads.
	Reverse string
	// Argv is the CLI command words (flags are built by the caller).
	Argv string
	Note string
}

const (
	gUser    = "https://www.googleapis.com/auth/admin.directory.user"
	gUserRO  = "https://www.googleapis.com/auth/admin.directory.user.readonly"
	gGroup   = "https://www.googleapis.com/auth/admin.directory.group"
	gGroupRO = "https://www.googleapis.com/auth/admin.directory.group.readonly"
	gOU      = "https://www.googleapis.com/auth/admin.directory.orgunit"
	gOURO    = "https://www.googleapis.com/auth/admin.directory.orgunit.readonly"
)

// Ops is the reviewed capability table.
var Ops = []Op{
	{"m365", "user-list", EffectRead, "User.Read.All", "", "", "entra user list", "CLI pages internally; result is bounded by gws.MaxTotal"},
	{"m365", "user-get", EffectRead, "User.Read.All", "", "", "entra user get", ""},
	{"m365", "group-list", EffectRead, "Group.Read.All", "", "", "entra group list", ""},
	{"m365", "user-add", EffectWrite, "User.ReadWrite.All", "", "user-remove", "entra user add", "UPN is unique: a replay is a typed conflict"},
	{"m365", "user-remove", EffectDelete, "User.ReadWrite.All", "", "", "entra user remove", "soft-deleted (recoverable) by Entra; the CLI's own --force prompt bypass is passed only after Validate"},
	{"m365", "group-add", EffectWrite, "Group.ReadWrite.All", "", "group-remove", "entra group add", ""},
	{"m365", "group-remove", EffectDelete, "Group.ReadWrite.All", "", "", "entra group remove", ""},

	{"gam", "user-list", EffectRead, gUserRO, "admin:users", "", "print users", "CSV output"},
	{"gam", "user-info", EffectRead, gUserRO, "admin:users", "", "info user", ""},
	{"gam", "group-list", EffectRead, gGroupRO, "admin:groups", "", "print groups", ""},
	{"gam", "orgunit-list", EffectRead, gOURO, "admin:orgunits", "", "print orgs", ""},
	{"gam", "user-create", EffectWrite, gUser, "admin:users", "user-delete", "create user", ""},
	{"gam", "user-delete", EffectDelete, gUser, "admin:users", "", "delete user", "recoverable for 20 days via undelete (vendor docs)"},
	{"gam", "group-create", EffectWrite, gGroup, "admin:groups", "group-delete", "create group", ""},
	{"gam", "group-delete", EffectDelete, gGroup, "admin:groups", "", "delete group", ""},
	{"gam", "orgunit-create", EffectWrite, gOU, "admin:orgunits", "orgunit-delete", "create org", ""},
	{"gam", "orgunit-delete", EffectDelete, gOU, "admin:orgunits", "", "delete org", ""},
}

// UnsupportedAdmin lists admin operations that are NOT admitted, with the
// reason; mirrored in EVIDENCE.md.
var UnsupportedAdmin = map[string]string{
	"gam:mobile":            "gws defers admin:mobile; GAM command mapping not reviewed — unclaimed",
	"gam:chromeos":          "gws defers admin:chromeos; GAM command mapping not reviewed — unclaimed",
	"gam:bulk":              "csv/loop/batch fan-out is mass targeting — refused (ModeBulk)",
	"gam:oauth-user-creds":  "only service-account domain-wide delegation maps onto the gws credential selection",
	"m365:raw-request":      "m365 request / cli config bypass the op table — refused (ModeRawAPI)",
	"m365:license-assign":   "license assignment changes billing: not admitted",
	"m365:role-assignment":  "privileged-role assignment is not admitted",
	"m365:spo-teams-exo":    "SharePoint/Teams/Exchange admin groups are out of this story's scope",
	"m365:device-code-auth": "interactive login refused (ModeInteractiveAuth); credentials are file-selected",
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

// Request is a validated-before-effects intent (read or mutation).
type Request struct {
	Scope   Scope
	Op      string
	Key     string
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

// Plan is the validated outcome of Request.Validate.
type Plan struct {
	Op  Op
	Key string
}

// Validate checks everything that can be checked before any effect: the op
// is admitted, the declared permissions include the op's scope, a read
// declares no write permission (least scope), a mutation targets a declared
// test tenant/domain, and the payload is bounded.
func (r Request) Validate() (Plan, *gws.Error) {
	target := r.Scope.Client + ":" + r.Op
	op, ok := LookupOp(r.Scope.Client, r.Op)
	if !ok {
		return Plan{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: operation %q is not admitted (known: %v)", r.Scope.Client, r.Op, OpNames(r.Scope.Client)), nil).WithOp("validate", target)
	}
	if !r.Scope.has(op.Scope) {
		return Plan{}, gws.NewError(gws.CodePermissionDenied, fmt.Sprintf("%s needs permission %s which is not in the declared scopes", target, op.Scope), nil).WithOp("validate", target)
	}
	if op.Effect == EffectRead {
		for _, s := range r.Scope.Scopes {
			if writeScope(r.Scope.Client, s) {
				return Plan{}, gws.NewError(gws.CodePermissionDenied, fmt.Sprintf("%s is a read but the declared scopes include write permission %s — declare least privilege", target, s), nil).WithOp("validate", target)
			}
		}
	} else if !r.Scope.Sandbox {
		return Plan{}, gws.NewError(gws.CodePermissionDenied, fmt.Sprintf("%s mutates a directory and target %q is not a declared test tenant/domain", target, r.Scope.Target), nil).WithOp("validate", target)
	}
	if len(r.Payload) > gws.MaxPayloadBytes {
		return Plan{}, gws.NewError(gws.CodeOutputLimit, fmt.Sprintf("%s: payload %d bytes exceeds %d", target, len(r.Payload), gws.MaxPayloadBytes), nil).WithOp("validate", target)
	}
	key := r.Key
	if key == "" {
		key = DeriveKey(r.Scope, r.Op, r.Payload)
	}
	return Plan{Op: op, Key: key}, nil
}

// Ledger records applied mutation keys so a replay is refused, not repeated.
// In-memory: a durable cross-process ledger is a named gap.
type Ledger struct {
	mu   sync.Mutex
	done map[string]string
}

// Begin claims a key; a key already claimed is a replay.
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

// Finish records confirmed|failed|unknown; failed releases the key.
func (l *Ledger) Finish(key, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if outcome == "failed" {
		delete(l.done, key)
		return
	}
	l.done[key] = outcome
}
