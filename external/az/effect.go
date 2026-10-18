// Dry runs for az mutations (Sprint 457 story bedc320542eb).
//
// ARM deployment mutations preview through `az deployment … --what-if`: a
// provider-validated preview that performs no mutation. Every other dry
// run refuses without execution (the shipped 446 `az` adapt row carries
// preview_level unsupported; the `az-deployment` row carries
// provider-validated-preview with `--what-if`). A successful preview is
// never execution evidence. Cancelling a local request cannot guarantee
// rollback of a remote write: every executed mutation reports confirmed,
// failed, or unknown outcome.
package az

import (
	"fmt"
	"io"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// DryRun is a nonexecuting preview of a mutation. For ARM deployments it
// resolves to --what-if (validated, mutating nothing); for anything else
// it refuses.
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
		return NewError(CodeInvalidInput, fmt.Sprintf("az: unknown service %q", d.Service), nil)
	}
	if !svc.Supports(OpDryRun) {
		return Unsupported("dry-run", d.Service+":"+d.Operation)
	}
	if d.Credentials.Subscription == "" {
		return NewError(CodeMissingCredentials, "az: dry run needs explicit credentials — resolve them before previewing", nil)
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
