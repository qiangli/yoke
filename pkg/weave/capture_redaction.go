package weave

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/secrets"
)

// weaveCaptureRedaction is created at an agent-output capture boundary. It
// snapshots the vault-rendered values already present in the launcher's
// environment; it never fetches or renders the vault itself.
type weaveCaptureRedaction struct {
	redactor *secrets.Redactor
}

func newWeaveCaptureRedaction(environ []string, diagnostics io.Writer) weaveCaptureRedaction {
	return newWeaveCaptureRedactionForNames(environ, secrets.VaultEnvNames(), diagnostics)
}

func newWeaveCaptureRedactionForNames(environ []string, names map[string]struct{}, diagnostics io.Writer) weaveCaptureRedaction {
	if len(names) == 0 {
		return weaveCaptureRedaction{redactor: secrets.NewRedactor()}
	}

	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}

	sortedNames := make([]string, 0, len(names))
	for name := range names {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	redactor := secrets.NewRedactor()
	for _, name := range sortedNames {
		value, ok := values[name]
		if !ok {
			weaveWarnCaptureRedactionInactive(diagnostics)
			return weaveCaptureRedaction{}
		}
		if err := redactor.Register(name, value); err != nil {
			weaveWarnCaptureRedactionInactive(diagnostics)
			return weaveCaptureRedaction{}
		}
	}
	return weaveCaptureRedaction{redactor: redactor}
}

func weaveWarnCaptureRedactionInactive(w io.Writer) {
	fmt.Fprintln(w, "weave start: WARNING: SECRET REDACTION INACTIVE — not all vault-rendered values could be registered; capture will continue unredacted")
}

// Writer never closes dst. This matches secrets.Redactor.Writer and lets the
// caller flush the redactor's retained tail before closing a log file.
func (c weaveCaptureRedaction) Writer(dst io.Writer) io.WriteCloser {
	if c.redactor == nil {
		return weaveCapturePassthrough{Writer: dst}
	}
	return c.redactor.Writer(dst)
}

type weaveCapturePassthrough struct {
	io.Writer
}

func (weaveCapturePassthrough) Close() error { return nil }

// weaveSynchronizedWriter serializes writes to command output streams. Cobra
// callers commonly point stdout and stderr at the same bytes.Buffer, while
// os/exec copies the child's two pipes concurrently. Separate redactors protect
// their own state, but they cannot protect a shared downstream writer.
type weaveSynchronizedWriter struct {
	mu  *sync.Mutex
	dst io.Writer
}

func (w weaveSynchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}
