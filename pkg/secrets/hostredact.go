package secrets

import (
	"io"
	"os"
)

// hostRedactor retains minted values for this process, including old values
// after rotation so delayed child output cannot reveal a retired credential.
var hostRedactor = func() *Redactor {
	r := NewRedactor()
	_ = r.SetShapeMode(ShapeMask)
	return r
}()

// HostRedactor returns the process's shared registry of app-generated secrets.
func HostRedactor() *Redactor { return hostRedactor }

// RegisterHostSecret registers a minted or loaded credential before use.
// Errors never include the supplied value. Re-registering a value is harmless.
func RegisterHostSecret(name, value string) error { return hostRedactor.Register(name, value) }

// NewHostRedactingWriter masks host and vault values plus credential forms.
// Registration remains live after writer creation. Close flushes its held tail
// without closing dst; callers must close it before closing their log file.
func NewHostRedactingWriter(dst io.Writer) io.WriteCloser {
	return EnvValueRedactor(os.Environ()).LiveWriter(dst)
}
