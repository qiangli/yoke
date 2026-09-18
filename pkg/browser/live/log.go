package live

import (
	"io"
	"log/slog"
	"sync/atomic"
)

// The live package used to log through the package-level `slog`
// default. That handler writes to the PROCESS's os.Stderr, which is
// not the stream a bashy builtin was given: the shell hands each tool
// its own rc.Out/rc.Err, so a caller doing
//
//	browser --mode live --json tabs list >/tmp/o 2>/tmp/e
//
// got clean JSON in /tmp/o, an empty /tmp/e — and the log line on the
// terminal anyway, uncapturable and unsuppressible. It fired on every
// invocation to announce "hub already owned by another process", which
// is the normal case, not news.
//
// So the sink is now explicit and DISCARDS by default. A long-running
// foreground command that genuinely wants logs (`bashy browser hub`)
// calls SetLogger with a handler writing to its own rc.Err.
var sink atomic.Pointer[slog.Logger]

func init() {
	SetLogger(nil)
}

// SetLogger installs the sink for live-mode diagnostics. Passing nil
// restores the discarding default.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	sink.Store(l)
}

// NewWriterLogger builds a plain text logger over w at info level —
// the shape `browser hub` wants for its own stderr.
func NewWriterLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func logInfo(msg string, args ...any) { sink.Load().Info(msg, args...) }
func logWarn(msg string, args ...any) { sink.Load().Warn(msg, args...) }
