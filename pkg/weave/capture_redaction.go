package weave

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/secrets"
)

// weaveCaptureRedaction is created at an agent-output capture boundary. It
// snapshots the vault-rendered values already present in the launcher's
// environment; it never fetches or renders the vault itself.
//
// Failure posture: redact every value that can be registered. A value that
// cannot be (absent from the environment, or shorter than
// secrets.MinRedactedValueLen) is reported by NAME and reason only, and capture
// continues with partial redaction; one unprotectable entry never strips
// protection from the others.
type weaveCaptureRedaction struct {
	redactor *secrets.Redactor
	// secretsRendered is true when the vault names any secret for this run,
	// whether or not each could be registered; capture files are then owner-only.
	secretsRendered bool
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
	var failed []string
	registered := 0
	for _, name := range sortedNames {
		value, ok := values[name]
		if !ok {
			failed = append(failed, name+" (not in environment)")
			continue
		}
		if err := redactor.Register(name, value); err != nil {
			failed = append(failed, name+" ("+weaveRegisterFailureReason(err)+")")
			continue
		}
		registered++
	}
	if len(failed) > 0 {
		weaveWarnCaptureRedactionIncomplete(diagnostics, registered, failed)
	}
	return weaveCaptureRedaction{redactor: redactor, secretsRendered: true}
}

func weaveRegisterFailureReason(err error) string {
	if errors.Is(err, secrets.ErrSecretTooShort) {
		return "value too short to redact"
	}
	return "cannot be registered"
}

func weaveWarnCaptureRedactionIncomplete(w io.Writer, registered int, failed []string) {
	level := "PARTIAL"
	if registered == 0 {
		level = "INACTIVE"
	}
	fmt.Fprintf(w, "weave start: WARNING: SECRET REDACTION %s — %d vault-rendered value(s) registered; NOT redacted in capture: %s\n",
		level, registered, strings.Join(failed, ", "))
}

// weaveOpenCaptureLog creates the capture log truncated. When secrets were
// rendered for the run the file is 0600 in a 0700 directory, chmod-ed
// explicitly because OpenFile/MkdirAll honour the mode only on creation.
func weaveOpenCaptureLog(logsDir, logPath string, secretsRendered bool) (*os.File, error) {
	dirMode, fileMode := os.FileMode(0o755), os.FileMode(0o600)
	if secretsRendered {
		dirMode = 0o700
	}
	if err := os.MkdirAll(logsDir, dirMode); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	if secretsRendered {
		if err := os.Chmod(logsDir, dirMode); err != nil {
			return nil, fmt.Errorf("restrict log dir: %w", err)
		}
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	if err := f.Chmod(fileMode); err != nil && runtime.GOOS != "windows" {
		_ = f.Close()
		return nil, fmt.Errorf("restrict log: %w", err)
	}
	return f, nil
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
