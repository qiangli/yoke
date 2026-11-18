package secrets

import (
	"io"
	"os"
)

// NewHostRecordWriter masks complete log records. Each Write must contain one
// complete record (as the standard log.Logger guarantees); use the streaming
// writer for arbitrary child output. It neither buffers nor closes dst, so a
// process can protect its inherited log descriptor without a parent pipe.
func NewHostRecordWriter(dst io.Writer) io.Writer {
	return &hostRecordWriter{dst: dst, redactor: EnvValueRedactor(os.Environ())}
}

type hostRecordWriter struct {
	dst      io.Writer
	redactor *Redactor
}

func (w *hostRecordWriter) Write(p []byte) (int, error) {
	if err := writeAll(w.dst, w.redactor.Redact(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
