// Dry runs for aws mutations (Sprint 457 story bedc320542eb).
//
// EC2 and S3 mutations preview through the provider's own dry-run flag
// (`--dry-run` / `--dryrun`): a provider-validated preview that performs
// no mutation. Every other dry run refuses without execution (the shipped
// 446 `aws` adapt row carries preview_level unsupported; the `aws-ec2`
// and `aws-s3` rows carry provider-validated-preview with their flag).
// A successful preview is never execution evidence. Cancelling a local
// request cannot guarantee rollback of a remote write: every executed
// mutation reports confirmed, failed, or unknown outcome.
package aws

import (
	"fmt"
	"io"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// DryRun is a nonexecuting preview of a mutation. For EC2/S3 it resolves
// to the provider flag (validated, mutating nothing); for anything else it
// refuses.
type DryRun struct {
	Service     string
	Operation   string
	Target      string
	Credentials Credentials
}

// Check validates the dry run without executing: service/operation
// support, explicit credentials (already resolved), and the preview level.
// It never mutates, even for provider-validated previews (those only read).
func (d DryRun) Check() *Error {
	svc, ok := LookupService(d.Service)
	if !ok {
		if Deferral(d.Service+":"+d.Operation) != "" {
			return Unsupported("dry-run", d.Service+":"+d.Operation)
		}
		return NewError(CodeInvalidInput, fmt.Sprintf("aws: unknown service %q", d.Service), nil)
	}
	if !svc.Supports(OpDryRun) {
		return Unsupported("dry-run", d.Service+":"+d.Operation)
	}
	if d.Credentials.Profile == "" {
		return NewError(CodeMissingCredentials, "aws: dry run needs explicit credentials — resolve them before previewing", nil)
	}
	return nil
}

// Emit writes the dry-run preview as a loom-v2 ok envelope: what WOULD
// happen, the preview level, the provider flag, the redacted scope, and
// the declared effects — never execution evidence.
func (d DryRun) Emit(w io.Writer, command string) int {
	if err := d.Check(); err != nil {
		return err.EmitError(w, command)
	}
	svc, _ := LookupService(d.Service)
	return weavecli.EmitOK(w, weavecli.OutputJSON, command, map[string]string{
		"preview":   string(PreviewProviderValidated),
		"operation": d.Operation,
		"target":    d.Target,
		"flag":      svc.DryRunFlag,
		"scope":     d.Credentials.RedactedScope(),
		"effects":   "net.egress(read-only validation) auth.refresh",
	})
}
