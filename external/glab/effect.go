// Dry runs for glab mutations (Sprint 457 story bedc320542eb).
//
// glab ships no dry-run mode: every mutation preview refuses without
// execution (the shipped 446 `glab` adapt row carries preview_level
// unsupported, same posture as `gh`/`gh-api`). A successful preview is
// never execution evidence. Cancelling a local request cannot guarantee
// rollback of a remote write: every executed mutation reports confirmed,
// failed, or unknown outcome.
package glab

import (
	"fmt"
	"io"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// DryRun is a nonexecuting preview of a mutation. glab supports no dry run,
// so Check always refuses with unsupported-capability and Emit surfaces the
// refusal as a loom-v2 error envelope — it never mutates, never reads.
type DryRun struct {
	Service     string
	Operation   string
	Target      string
	Credentials Credentials
}

// Check validates the dry run without executing. glab has no preview mode,
// so this refuses for every service/operation, including admitted ones.
func (d DryRun) Check() *Error {
	svc, ok := LookupService(d.Service)
	if !ok {
		if Deferral(d.Service+":"+d.Operation) != "" {
			return Unsupported("dry-run", d.Service+":"+d.Operation)
		}
		return NewError(CodeInvalidInput, fmt.Sprintf("glab: unknown service %q", d.Service), nil)
	}
	_ = svc
	return Unsupported("dry-run", d.Service+":"+d.Operation)
}

// Emit writes the refusal as a loom-v2 error envelope: what WOULD happen
// is not previewable through glab, so callers must not treat any output
// here as execution evidence.
func (d DryRun) Emit(w io.Writer, command string) int {
	if err := d.Check(); err != nil {
		return err.EmitError(w, command)
	}
	return weavecli.EmitOK(w, weavecli.OutputJSON, command, map[string]string{
		"preview":   string(PreviewUnsupported),
		"operation": d.Operation,
		"target":    d.Target,
		"scope":     d.Credentials.RedactedScope(),
	})
}
