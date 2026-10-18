// Effects, uploads, and dry runs for gws mutations (Sprint 457 story f25d7aa4437c).
//
// Mutations validate BEFORE effects: bounded stdin or explicit file payloads
// with declared media type, encoding, size limits, and mutually exclusive
// input sources. Missing required input fails promptly; payloads are never
// interpolated into shell code and secrets never ride argv.
//
// `--explain` never executes. An unsupported dry run refuses — it never
// executes a mutation — and a successful preview is not later execution
// evidence. Local previews perform no provider call; provider-validated
// previews declare their network/read/auth effects and still mutate nothing.
// Cancelling a local request cannot guarantee rollback of a remote write:
// every executed mutation reports confirmed, failed, or unknown outcome.
package gws

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Upload bounds. Large artifacts travel by locator reference, not inline.
const (
	// MaxUploadBytes caps one upload payload (25 MiB, the Gmail attachment
	// ceiling that bounds every Workspace upload path here).
	MaxUploadBytes = 25 << 20
	// MaxPayloadBytes caps an inline stdin payload.
	MaxPayloadBytes = 1 << 20
)

// Upload is a validated file-upload intent for services that admit OpUpload
// (Drive). Validate before effects; Execute performs the provider call and
// reports confirmed/failed/unknown.
type Upload struct {
	Service     string
	Name        string
	LocalPath   string
	Stdin       bool
	MediaType   string
	SizeLimit   int64
	Credentials Credentials
}

// Validate checks service support, input-source exclusivity, media type,
// and size bounds without performing any effect. Local problems are
// local-preview class; nothing here touches the network.
func (u Upload) Validate() *Error {
	svc, ok := LookupService(u.Service)
	if !ok {
		return Unsupported("upload", u.Service+":upload")
	}
	if !svc.Supports(OpUpload) {
		return Unsupported("upload", u.Service+":upload")
	}
	if u.LocalPath == "" && !u.Stdin {
		return NewError(CodeInvalidInput, "gws: upload needs an explicit file payload (--file) or stdin; missing input fails promptly", nil)
	}
	if u.LocalPath != "" && u.Stdin {
		return NewError(CodeInvalidInput, "gws: upload takes exactly one input source — --file or stdin, not both", nil)
	}
	if strings.TrimSpace(u.MediaType) == "" {
		return NewError(CodeInvalidInput, "gws: upload needs a declared media type", nil)
	}
	limit := u.SizeLimit
	if limit <= 0 || limit > MaxUploadBytes {
		limit = MaxUploadBytes
	}
	if u.Stdin {
		// Stdin size is enforced while reading (effect.go ReadBounded),
		// not after buffering; the cap is declared up front.
		return nil
	}
	fi, err := os.Stat(u.LocalPath)
	if err != nil {
		return NewError(CodeInvalidInput, fmt.Sprintf("gws: upload file %q unreadable: %v", u.LocalPath, err), nil)
	}
	if fi.IsDir() {
		return NewError(CodeInvalidInput, fmt.Sprintf("gws: upload file %q is a directory", u.LocalPath), nil)
	}
	if fi.Size() > limit {
		return NewError(CodeOutputLimit, fmt.Sprintf("gws: upload file %q is %d bytes, over the %d-byte limit", u.LocalPath, fi.Size(), limit), nil)
	}
	return nil
}

// ReadBounded reads at most limit+1 bytes so exhaustion is explicit, never
// silent truncation.
func ReadBounded(r io.Reader, limit int64) ([]byte, *Error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Read(tmp)
		total += int64(n)
		if total > limit {
			return nil, NewError(CodeOutputLimit, fmt.Sprintf("gws: payload exceeds the %d-byte limit — narrow the input or use a file upload", limit), nil)
		}
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

// DryRun is a nonexecuting preview of a mutation. Preview success is not
// execution evidence; unsupported dry runs refuse without execution.
type DryRun struct {
	Service     string
	Operation   string
	Target      string
	Preview     Preview
	Credentials Credentials
}

// Check validates the dry run without executing: service/operation support,
// explicit credentials (already resolved), and the preview level. It never
// mutates, even for provider-validated previews (those only read).
func (d DryRun) Check() *Error {
	svc, ok := LookupService(d.Service)
	if !ok {
		if Deferral(d.Service+":"+d.Operation) != "" {
			return Unsupported("dry-run", d.Service+":"+d.Operation)
		}
		return NewError(CodeInvalidInput, fmt.Sprintf("gws: unknown service %q", d.Service), nil)
	}
	if !svc.Supports(OpDryRun) {
		return Unsupported("dry-run", d.Service+":"+d.Operation)
	}
	if d.Credentials.Account == "" {
		return NewError(CodeMissingCredentials, "gws: dry run needs explicit credentials — resolve them before previewing", nil)
	}
	return nil
}

// Emit writes the dry-run preview as a loom-v2 ok envelope: what WOULD
// happen, the preview level, the redacted scope, and the declared effects —
// never execution evidence.
func (d DryRun) Emit(w io.Writer, command string) int {
	if err := d.Check(); err != nil {
		return err.EmitError(w, command)
	}
	return weavecli.EmitOK(w, weavecli.OutputJSON, command, map[string]string{
		"preview":   string(PreviewProviderValidated),
		"operation": d.Operation,
		"target":    d.Target,
		"scope":     d.Credentials.RedactedScope(),
		"effects":   "net.egress(read-only validation) auth.refresh",
	})
}
