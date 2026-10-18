// Service capability mapping for glab (Sprint 457 story bedc320542eb).
//
// This is 457-owned service-specific data: which GitLab areas glab fronts,
// which operation classes each supports, and what is deferred or unclaimed.
// It is consumed by 446 adapt rows and declared lens parsers — it is NOT a
// competing registry, parser, or lease mechanism, and it defines no wire
// schema of its own. Pagination, envelopes, and error fields are owned by
// 446 (loom-v2 native envelope, bashy-cursor-v1); see page.go and errors.go
// for the consumption points.
package glab

// Preview is the 446 four-level preview declaration. A local preview
// performs no provider/network call; a provider-validated preview may use
// declared network/read/auth access but performs no mutation; unsupported
// refuses without execution.
type Preview string

const (
	PreviewExplainOnly       Preview = "explain-only"
	PreviewLocal             Preview = "local-preview"
	PreviewProviderValidated Preview = "provider-validated-preview"
	PreviewUnsupported       Preview = "unsupported"
)

// OpClass names a capability class inside a service.
type OpClass string

const (
	OpRead   OpClass = "read"    // list/get bounded reads with pagination
	OpWrite  OpClass = "write"   // reversible create/update with cleanup
	OpDryRun OpClass = "dry-run" // nonexecuting preview of a mutation
)

// Service describes one GitLab area fronted through glab.
type Service struct {
	Name string
	// Ops lists the supported operation classes for this service.
	Ops []OpClass
	// DryRunPreview is the preview level a dry run of a mutation gets.
	// glab ships no dry-run mode: every service refuses.
	DryRunPreview Preview
	// Note carries service-specific admission remarks.
	Note string
}

// Services is the admitted set: issues, merge requests, and pipelines.
// Every mutation requires explicit credentials (auth.go). Dry runs refuse
// everywhere (effect.go); login never implies provisioning permission.
var Services = []Service{
	{Name: "issues", Ops: []OpClass{OpRead, OpWrite}, DryRunPreview: PreviewUnsupported, Note: "issue list/get; create/close round-trip is the reversible-write fixture"},
	{Name: "merge-requests", Ops: []OpClass{OpRead, OpWrite}, DryRunPreview: PreviewUnsupported, Note: "MR list/get; comment/create round-trip only in a test project with cleanup"},
	{Name: "pipelines", Ops: []OpClass{OpRead}, DryRunPreview: PreviewUnsupported, Note: "read-only; triggering or cancelling pipelines is not admitted here"},
}

// Deferred records capabilities that are NOT absent but belong elsewhere:
// pipeline trigger/cancel needs durable-event semantics, and instance
// administration stays unclaimed until tested.
var Deferred = map[string]string{
	"pipelines:trigger": "deferred to the durable-event story (long-running pipeline supervision needs lease renewal evidence)",
	"pipelines:cancel":  "deferred to the durable-event story (cancellation of a running pipeline is event-story acceptance)",
	"admin:instance":    "UNCLAIMED until tested (instance administration is a separate story; never infer it from login)",
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
