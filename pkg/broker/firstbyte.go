package broker

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// The local engine's first-byte bound is separate from the remote 30 s
// fail-fast (gateway.DefaultFirstByteTimeout): prompt prefill on a
// CPU-only host legitimately takes longer than 30 s, while a dead metered
// key must never hang a caller. The door supervises the engine process, so
// engine death fails fast through the exit watch in proxyEngine instead of
// through this bound.

// DefaultLocalFirstByteTimeout bounds how long the local engine may stay
// silent before its first response byte when nothing else is configured.
const DefaultLocalFirstByteTimeout = 10 * time.Minute

// LocalFirstByteEnv overrides the local first-byte bound with a Go duration
// ("90s", "10m"); "0" (or a negative value) disables the bound entirely and
// the request context is the only deadline. Empty or unparsable selects the
// default.
const LocalFirstByteEnv = "BASHY_LLM_LOCAL_FIRST_BYTE"

// noLocalFirstByte disables the local first-byte bound: pass it (or any
// negative duration) as Options.LocalFirstByte.
const noLocalFirstByte = time.Duration(-1)

func parseLocalFirstByte(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if s == "0" || strings.HasPrefix(s, "-") ||
		strings.EqualFold(s, "off") || strings.EqualFold(s, "disable") ||
		strings.EqualFold(s, "disabled") || strings.EqualFold(s, "no") ||
		strings.EqualFold(s, "none") || strings.EqualFold(s, "inf") {
		return noLocalFirstByte, true
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return noLocalFirstByte, true
		}
		return d, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second)), true
	}
	return 0, false
}

func localFirstByteFromEnv() time.Duration {
	if d, ok := parseLocalFirstByte(os.Getenv(LocalFirstByteEnv)); ok {
		return d
	}
	return DefaultLocalFirstByteTimeout
}

// localFirstByte resolves the bound proxyEngine applies to the local
// engine: the Options override, else the environment, else the default. A
// negative value disables the bound (the request context still applies).
func (b *Broker) localFirstByte() time.Duration {
	if b.opts.LocalFirstByte != 0 {
		return b.opts.LocalFirstByte
	}
	return localFirstByteFromEnv()
}

// engineExiter is an optional Engine capability: the channel closes when
// the engine process exits. ExecEngine implements it; a static (already
// running) engine has no process to watch.
type engineExiter interface {
	Exited() <-chan struct{}
}

// engineExited returns the engine-death signal, or nil when the engine
// does not report one.
func (b *Broker) engineExited() <-chan struct{} {
	e, ok := b.opts.Engine.(engineExiter)
	if !ok || e == nil {
		return nil
	}
	return e.Exited()
}
