// Service capability mapping for az (Sprint 457 story bedc320542eb).
//
// This is 457-owned service-specific data: which Azure areas this package
// fronts, which operation classes each supports, the provider dry-run flag
// where one exists, and what stays unclaimed. It is consumed by 446 adapt
// rows and declared lens parsers — it is NOT a competing registry, parser,
// or lease mechanism, and it defines no wire schema of its own.
// Pagination, envelopes, and error fields are owned by 446 (loom-v2 native
// envelope, bashy-cursor-v1); see page.go and errors.go for the
// consumption points.
package az

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
	OpRead   OpClass = "read"    // list/show bounded reads with pagination
	OpWrite  OpClass = "write"   // reversible create/update with cleanup
	OpDryRun OpClass = "dry-run" // nonexecuting preview of a mutation
)

// Service describes one Azure area fronted through az.
type Service struct {
	Name string
	// Ops lists the supported operation classes for this service.
	Ops []OpClass
	// DryRunPreview is the preview level a dry run of a mutation gets.
	DryRunPreview Preview
	// DryRunFlag is the provider flag a validated preview appends
	// ("--what-if" for ARM deployments); "" when unsupported.
	DryRunFlag string
	// Note carries service-specific admission remarks.
	Note string
}

// Services is the admitted set: resource groups, virtual machines, and ARM
// deployments. Deployment mutations preview through `az deployment …
// --what-if` (validated, mutating nothing); group and vm dry runs refuse.
// Every mutation requires explicit credentials (auth.go).
var Services = []Service{
	{Name: "group", Ops: []OpClass{OpRead, OpWrite}, DryRunPreview: PreviewUnsupported, Note: "group list/show; create/delete round-trip is the reversible-write fixture"},
	{Name: "vm", Ops: []OpClass{OpRead, OpWrite}, DryRunPreview: PreviewUnsupported, Note: "vm list/show; create/delete round-trip only in a test group with cleanup"},
	{Name: "deployment", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, DryRunFlag: "--what-if", Note: "ARM template what-if preview before any deployment write"},
}

// Unclaimed records capabilities that stay unclaimed: subscription-level
// provisioning is never inferred from login.
var Unclaimed = map[string]string{
	"subscription:create": "UNCLAIMED until tested (subscription-level provisioning is never inferred from login)",
	"role:assign":         "UNCLAIMED until tested (role assignments need a separate authorization story)",
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
func Deferral(key string) string { return Unclaimed[key] }
