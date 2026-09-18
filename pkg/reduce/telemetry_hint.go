// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import "bytes"

const (
	// These limits bound classifier state independently of capture size. Once
	// either limit is reached the reducer fails open: the line is retained.
	maxTelemetryHintBytes = 4 * 1024
	maxRememberedHints    = 64
)

var telemetryHintClassifiers = [...]func([]byte) bool{
	isTelemetryStartupHint,
}

// IsTelemetryHint reports whether line belongs to the closed set of
// machine-generated telemetry hints that are safe to deduplicate. It is
// deliberately not a fuzzy search: ordinary lines mentioning telemetry are
// data and must remain untouched.
func IsTelemetryHint(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 || len(line) > maxTelemetryHintBytes {
		return false
	}
	for _, classify := range telemetryHintClassifiers {
		if classify(line) {
			return true
		}
	}
	return false
}

func isTelemetryStartupHint(line []byte) bool {
	const prefix = "bashy: telemetry on → "
	const servicePrefix = " (service="
	const disabledPrefix = "bashy: telemetry disabled — "
	if bytes.HasPrefix(line, []byte(disabledPrefix)) {
		return printableTelemetryPayload(line[len(disabledPrefix):])
	}
	if !bytes.HasPrefix(line, []byte(prefix)) || !bytes.HasSuffix(line, []byte(")")) {
		return false
	}
	rest := line[len(prefix):]
	at := bytes.LastIndex(rest, []byte(servicePrefix))
	if at <= 0 {
		return false
	}
	for _, c := range rest[:at] {
		if c < ' ' || c == 0x7f {
			return false
		}
	}
	service := rest[at+len(servicePrefix) : len(rest)-1]
	if len(service) == 0 {
		return false
	}
	for _, c := range service {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func printableTelemetryPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	for _, c := range payload {
		if c < ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

type telemetryHintDeduper struct {
	out             []byte
	pending         []byte
	seen            map[string]struct{}
	lineTooLong     bool
	suppressed      int
	suppressedBytes int
}

// write accepts arbitrary capture chunks. pending is capped; an overlong line
// becomes passthrough immediately and is never retained as a comparison key.
func (d *telemetryHintDeduper) write(chunk []byte) {
	for len(chunk) > 0 {
		at := bytes.IndexByte(chunk, '\n')
		if at < 0 {
			d.writeFragment(chunk)
			return
		}
		d.writeFragment(chunk[:at+1])
		chunk = chunk[at+1:]
		d.finishLine()
	}
}

func (d *telemetryHintDeduper) writeFragment(fragment []byte) {
	if d.lineTooLong {
		d.out = append(d.out, fragment...)
		return
	}
	if len(d.pending)+len(fragment) > maxTelemetryHintBytes+2 { // room for CRLF
		d.out = append(d.out, d.pending...)
		d.out = append(d.out, fragment...)
		d.pending = d.pending[:0]
		d.lineTooLong = true
		return
	}
	d.pending = append(d.pending, fragment...)
}

func (d *telemetryHintDeduper) finishLine() {
	if d.lineTooLong {
		d.lineTooLong = false
		return
	}
	d.finishPending()
}

func (d *telemetryHintDeduper) finishPending() {
	if len(d.pending) == 0 {
		return
	}
	line := d.pending
	d.pending = nil
	if !IsTelemetryHint(line) {
		d.out = append(d.out, line...)
		return
	}
	key := string(line) // canonicalized and redacted before this stage
	if _, ok := d.seen[key]; ok {
		d.suppressed++
		d.suppressedBytes += len(line)
		return
	}
	if len(d.seen) >= maxRememberedHints {
		// Exactness wins over reduction when the bounded registry is full.
		d.out = append(d.out, line...)
		return
	}
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	d.seen[key] = struct{}{}
	d.out = append(d.out, line...)
}

func (d *telemetryHintDeduper) finish() ([]byte, int, int) {
	if d.lineTooLong {
		d.lineTooLong = false
	} else {
		d.finishPending()
	}
	return d.out, d.suppressed, d.suppressedBytes
}

func deduplicateTelemetryHints(in []byte) ([]byte, int, int) {
	d := telemetryHintDeduper{out: make([]byte, 0, len(in))}
	d.write(in)
	return d.finish()
}
