// Service capability mapping for gws (Sprint 457 story f25d7aa4437c).
//
// This is 457-owned service-specific data: which Workspace services gws
// fronts, which operation classes each supports, and what is deferred or
// unclaimed. It is consumed by 446 adapt rows and declared lens parsers —
// it is NOT a competing registry, parser, or lease mechanism, and it defines
// no wire schema of its own. Pagination, envelopes, and error fields are
// owned by 446 (loom-v2 native envelope, bashy-cursor-v1); see page.go and
// errors.go for the consumption points.
package gws

// Preview is the 446 four-level preview declaration. A local preview
// performs no provider/network call; a provider-validated preview may use
// declared network/read/auth access but performs no mutation; unsupported
// refuses without execution.
type Preview string

const (
	PreviewExplainOnly          Preview = "explain-only"
	PreviewLocal                Preview = "local-preview"
	PreviewProviderValidated    Preview = "provider-validated-preview"
	PreviewUnsupported          Preview = "unsupported"
	PreviewDeferredToEventStory Preview = "deferred-to-event-story"
	PreviewUnclaimed            Preview = "unclaimed"
)

// OpClass names a capability class inside a service.
type OpClass string

const (
	OpRead      OpClass = "read"       // list/get bounded reads with pagination
	OpWrite     OpClass = "write"      // reversible create/update with cleanup
	OpUpload    OpClass = "upload"     // bounded file/stdin payload upload
	OpDryRun    OpClass = "dry-run"    // nonexecuting preview of a mutation
	OpWatch     OpClass = "watch"      // long-lived watch/subscription
	OpAdminCRUD OpClass = "admin-crud" // Admin Directory CRUD
)

// Service describes one Workspace service fronted through gws.
type Service struct {
	Name string
	// Ops lists the supported operation classes for this service.
	Ops []OpClass
	// DryRunPreview is the preview level a dry run of a mutation gets.
	DryRunPreview Preview
	// Note carries service-specific admission remarks.
	Note string
}

// Services is the admitted service set: Drive, Gmail, Calendar, Sheets,
// Docs, Chat, Slides, Tasks, Contacts. Uploads and dry runs are supported
// per the Ops flags; every mutation requires explicit credentials (auth.go)
// and honors effect guards (effect.go).
var Services = []Service{
	{Name: "drive", Ops: []OpClass{OpRead, OpWrite, OpUpload, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "file upload with bounded payload, media type, and size limits"},
	{Name: "gmail", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "send/draft are reversible-testable only with an authorized test account; watch is deferred"},
	{Name: "calendar", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "event create/delete round-trip is the reversible-write fixture"},
	{Name: "sheets", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "bounded cell ranges; declare precision/null preservation"},
	{Name: "docs", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "bounded document reads; structured edits only"},
	{Name: "chat", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "never send real messages as fixtures; test-space posts only with cleanup"},
	{Name: "slides", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "bounded presentation reads; structured edits only"},
	{Name: "tasks", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "task create/delete round-trip is the reversible-write fixture"},
	{Name: "contacts", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, Note: "no real user data as fixtures; test contacts only with cleanup"},
}

// Deferred records capabilities that are NOT absent but belong elsewhere:
// Gmail watch and Events subscribe/renew are subject to the durable-event
// story 5548854717bc, and Admin Directory CRUD stays unclaimed until tested.
var Deferred = map[string]string{
	"gmail:watch":      "event story 5548854717bc: use external/gws/events (opt-in checkpoint, lease renewal; hosted receiver needs service Rule 1; live renewal unverified until Sprint 465)",
	"events:subscribe": "event story 5548854717bc: use external/gws/events (checkpoint, dedup, restart recovery; live subscription unverified until Sprint 465)",
	"events:renew":     "event story 5548854717bc: use external/gws/events lease renewal (fakes only; live renewal unverified until Sprint 465)",
	"admin:directory":  "UNCLAIMED until tested (separate story 66494446f180 owns admin coverage; GAM overlap retained)",
	"admin:users":      "UNCLAIMED until tested (see admin:directory)",
	"admin:groups":     "UNCLAIMED until tested (see admin:directory)",
	"admin:orgunits":   "UNCLAIMED until tested (see admin:directory)",
	"admin:mobile":     "UNCLAIMED until tested (see admin:directory)",
	"admin:chromeos":   "UNCLAIMED until tested (see admin:directory)",
}

// LookupService returns the admitted service record, if any.
func LookupService(name string) (Service, bool) {
	for _, s := range Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// Supports reports whether the service admits the operation class.
func (s Service) Supports(op OpClass) bool {
	for _, o := range s.Ops {
		if o == op {
			return true
		}
	}
	return false
}

// Deferral explains why a "service:capability" is not implemented here,
// or "" when it is neither admitted nor deferred (i.e. unsupported input).
func Deferral(key string) string { return Deferred[key] }
