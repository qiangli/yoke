// Service capability mapping for aws (Sprint 457 story bedc320542eb).
//
// This is 457-owned service-specific data: which AWS areas this package
// fronts, which operation classes each supports, the provider dry-run flag
// where one exists, and what is unclaimed. It is consumed by 446 adapt
// rows and declared lens parsers — it is NOT a competing registry, parser,
// or lease mechanism, and it defines no wire schema of its own.
// Pagination, envelopes, and error fields are owned by 446 (loom-v2 native
// envelope, bashy-cursor-v1); see page.go and errors.go for the
// consumption points.
package aws

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
	OpRead   OpClass = "read"    // list/describe bounded reads with pagination
	OpWrite  OpClass = "write"   // reversible create/update with cleanup
	OpDryRun OpClass = "dry-run" // nonexecuting preview of a mutation
)

// Service describes one AWS area fronted through aws.
type Service struct {
	Name string
	// Ops lists the supported operation classes for this service.
	Ops []OpClass
	// DryRunPreview is the preview level a dry run of a mutation gets.
	DryRunPreview Preview
	// DryRunFlag is the provider flag a validated preview appends
	// ("--dry-run" for EC2, "--dryrun" for S3); "" when unsupported.
	DryRunFlag string
	// Note carries service-specific admission remarks.
	Note string
}

// Services is the admitted set: EC2, S3, and IAM. EC2 and S3 mutations
// preview through the provider's own dry-run flag (validated, mutating
// nothing); IAM is read-only here. Every mutation requires explicit
// credentials (auth.go).
var Services = []Service{
	{Name: "ec2", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, DryRunFlag: "--dry-run", Note: "describe-* reads; run-instances/terminate round-trip is the reversible-write fixture (dry-run first)"},
	{Name: "s3", Ops: []OpClass{OpRead, OpWrite, OpDryRun}, DryRunPreview: PreviewProviderValidated, DryRunFlag: "--dryrun", Note: "ls/cp round-trip in a test bucket with cleanup (dryrun first)"},
	{Name: "iam", Ops: []OpClass{OpRead}, DryRunPreview: PreviewUnsupported, Note: "read-only; identity and policy writes are never inferred from login"},
}

// Unclaimed records capabilities that are NOT absent but stay unclaimed:
// IAM writes need a separate authorization story, and account-level
// provisioning is never inferred from login.
var Unclaimed = map[string]string{
	"iam:write":   "UNCLAIMED until tested (identity/policy writes need a separate authorization story; never infer from login)",
	"account:set": "UNCLAIMED until tested (account-level provisioning is never inferred from login)",
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
