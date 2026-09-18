// Package secrets includes best-effort output masking for values registered
// with Redactor. Value masking stops accidental direct emission; it is not a
// secrecy guarantee. Transforming a value (for example with base64, reversal,
// or separately printed pieces) defeats exact-value masking. GitHub Actions
// uses the same general scheme and documents comparable evasions.
package secrets

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"unicode/utf8"
)

const (
	// MinRedactedValueLen is the shortest value Redactor accepts. Shorter
	// strings occur too often in ordinary output to mask safely.
	MinRedactedValueLen = 8

	maxShapeMatchLen = 256
)

var (
	// ErrSecretTooShort is returned when a value is too short to redact safely.
	// Its text deliberately does not contain the rejected value.
	ErrSecretTooShort = errors.New("secret value is shorter than the redaction minimum")

	errInvalidSecretName = errors.New("secret name is empty or contains unsupported characters")
	errInvalidShapeMode  = errors.New("invalid credential shape mode")
	errRedactorClosed    = errors.New("redacting writer is closed")
)

// ShapeMode controls what Redactor does with credential-shaped text that was
// not registered as a known value.
type ShapeMode uint8

const (
	// ShapeReportOnly is the default. Matches are reported to the configured
	// ShapeReporter but output is not changed, because shape matching can have
	// false positives.
	ShapeReportOnly ShapeMode = iota
	// ShapeMask replaces credential-shaped text as well as registered values.
	ShapeMask
)

// ShapeMatch describes a credential-shaped substring without retaining or
// exposing the substring itself.
type ShapeMatch struct {
	Kind   string
	Offset int
	Length int
}

// Redactor masks registered secret values in byte streams. Register all known
// values before creating a Writer; each Writer uses an immutable snapshot of
// the registry and shape settings from the time it is created.
type Redactor struct {
	mu       sync.RWMutex
	secrets  []registeredSecret
	matcher  *valueMatcher
	shape    ShapeMode
	reporter func(ShapeMatch)
}

type registeredSecret struct {
	value       []byte
	replacement []byte
}

// NewRedactor returns an empty Redactor. Credential-shape detection defaults
// to report-only; install a reporter with SetShapeReporter to receive warnings.
func NewRedactor() *Redactor {
	return &Redactor{shape: ShapeReportOnly}
}

// Register adds a named secret value. Values shorter than
// MinRedactedValueLen are refused to avoid corrupting ordinary output.
// Returned errors never include value.
func (r *Redactor) Register(name, value string) error {
	if utf8.RuneCountInString(value) < MinRedactedValueLen {
		return ErrSecretTooShort
	}
	if !validSecretName(name) {
		return errInvalidSecretName
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.secrets = append(r.secrets, registeredSecret{
		value:       []byte(value),
		replacement: []byte("[redacted:" + name + "]"),
	})
	r.matcher = newValueMatcher(r.secrets)
	return nil
}

// SetShapeMode selects report-only or masking behavior for credential-shaped
// text which was not registered as a known value.
func (r *Redactor) SetShapeMode(mode ShapeMode) error {
	if mode != ShapeReportOnly && mode != ShapeMask {
		return errInvalidShapeMode
	}
	r.mu.Lock()
	r.shape = mode
	r.mu.Unlock()
	return nil
}

// SetShapeReporter installs a callback for credential-shape warnings. A
// ShapeMatch contains only a kind and byte location, never the matched text.
// Passing nil disables callbacks while leaving shape detection report-only.
func (r *Redactor) SetShapeReporter(reporter func(ShapeMatch)) {
	r.mu.Lock()
	r.reporter = reporter
	r.mu.Unlock()
}

// Redact returns a copy of b with registered values masked. Credential-shaped
// text is reported but, by default, is not changed.
func (r *Redactor) Redact(b []byte) []byte {
	cfg := r.snapshot()
	out, _, findings := cfg.transformPrefix(b, len(b))
	cfg.report(findings)
	return out
}

// Writer returns a streaming redactor. It retains enough unprocessed input to
// recognize a secret split across Write calls and flushes that tail on Close.
// Closing it does not close the underlying writer.
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	return &redactingWriter{dst: w, cfg: r.snapshot()}
}

// DetectCredentialShapes finds common credential shapes without returning the
// matching text. Shape detection is intentionally a warning signal: callers
// should expect false positives and should not silently alter output from this
// function alone.
func DetectCredentialShapes(b []byte) []ShapeMatch {
	return detectCredentialShapes(b)
}

func validSecretName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !asciiAlphaNum(c) && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

type redactorConfig struct {
	matcher  *valueMatcher
	shape    ShapeMode
	reporter func(ShapeMatch)
	tailLen  int
}

func (r *Redactor) snapshot() redactorConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	maxLen := 0
	if r.matcher != nil {
		maxLen = r.matcher.maxLen
	}
	if (r.shape == ShapeMask || r.reporter != nil) && maxShapeMatchLen > maxLen {
		maxLen = maxShapeMatchLen
	}
	tailLen := 0
	if maxLen > 0 {
		tailLen = maxLen - 1
	}
	return redactorConfig{
		matcher:  r.matcher,
		shape:    r.shape,
		reporter: r.reporter,
		tailLen:  tailLen,
	}
}

type redactingWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	cfg     redactorConfig
	pending []byte
	closed  bool
	err     error
	offset  int
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, errRedactorClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	combined := make([]byte, 0, len(w.pending)+len(p))
	combined = append(combined, w.pending...)
	combined = append(combined, p...)
	boundary := len(combined) - w.cfg.tailLen
	if boundary < 0 {
		boundary = 0
	}

	out, consumed, findings := w.cfg.transformPrefix(combined, boundary)
	if err := writeAll(w.dst, out); err != nil {
		w.err = err
		return 0, err
	}
	w.report(findings)
	w.pending = append(w.pending[:0], combined[consumed:]...)
	w.offset += consumed
	return len(p), nil
}

func (w *redactingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}

	out, _, findings := w.cfg.transformPrefix(w.pending, len(w.pending))
	if err := writeAll(w.dst, out); err != nil {
		w.err = err
		return err
	}
	w.report(findings)
	w.pending = nil
	return nil
}

func (w *redactingWriter) report(findings []ShapeMatch) {
	if w.cfg.reporter == nil {
		return
	}
	for _, finding := range findings {
		finding.Offset += w.offset
		w.cfg.reporter(finding)
	}
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type matchCandidate struct {
	end         int
	replacement []byte
	exact       bool
}

func (cfg redactorConfig) transformPrefix(b []byte, boundary int) ([]byte, int, []ShapeMatch) {
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(b) {
		boundary = len(b)
	}

	// Matches are normally sparse. A dense []matchCandidate costs roughly
	// 40 bytes for every input byte even when the chunk contains no secret,
	// turning a 1 MiB capture write into a ~40 MiB allocation. Keep only match
	// starts instead; the output buffer is then the only input-sized allocation
	// on the common path.
	best := make(map[int]matchCandidate)
	if cfg.matcher != nil {
		cfg.matcher.addCandidates(b, best)
	}

	var shapes []ShapeMatch
	if cfg.shape == ShapeMask || cfg.reporter != nil {
		shapes = detectCredentialShapes(b)
	}
	if cfg.shape == ShapeMask {
		for _, finding := range shapes {
			candidate := matchCandidate{
				end:         finding.Offset + finding.Length,
				replacement: []byte("[redacted:" + finding.Kind + "]"),
			}
			setCandidate(best, finding.Offset, candidate)
		}
	}

	out := make([]byte, 0, boundary)
	pos := 0
	for pos < boundary {
		candidate := best[pos]
		if candidate.end > pos {
			// MERGE OVERLAPPING MATCHES. Taking this match and jumping straight to
			// its end leaks the TAIL of any registered value that starts inside it
			// and reaches further: with secrets "AAAAbbbb…" at 0 and "bbbb…CCCC" at
			// 4, consuming 0..15 emits "CCCC" — real secret material — in plaintext.
			// So extend the redacted span across every match that begins within it.
			// `end` grows as we scan, and the loop condition re-reads it, so a chain
			// of overlaps collapses into one span. `pos` then jumps past the whole
			// span, making this linear overall rather than quadratic.
			end := candidate.end
			for scan := pos + 1; scan < end; scan++ {
				if best[scan].end > end {
					end = best[scan].end
				}
			}
			// One placeholder names the value that STARTS the span. A merged span can
			// involve several secrets; naming the first keeps the output stable, and
			// an operator who sees it should treat every overlapping value as exposed.
			out = append(out, candidate.replacement...)
			pos = end
			continue
		}
		out = append(out, b[pos])
		pos++
	}

	reported := shapes[:0]
	for _, finding := range shapes {
		if finding.Offset < pos {
			reported = append(reported, finding)
		}
	}
	return out, pos, reported
}

func (cfg redactorConfig) report(findings []ShapeMatch) {
	if cfg.reporter == nil {
		return
	}
	for _, finding := range findings {
		cfg.reporter(finding)
	}
}

func setCandidate(best map[int]matchCandidate, start int, candidate matchCandidate) {
	if start < 0 {
		return
	}
	current := best[start]
	if current.end == 0 ||
		(candidate.exact && !current.exact) ||
		(candidate.exact == current.exact && candidate.end > current.end) {
		best[start] = candidate
	}
}

type valueMatcher struct {
	nodes  []matcherNode
	values []registeredSecret
	maxLen int
}

type matcherNode struct {
	next map[byte]int
	fail int
	out  []int
}

func newValueMatcher(values []registeredSecret) *valueMatcher {
	m := &valueMatcher{
		nodes:  []matcherNode{{next: make(map[byte]int)}},
		values: append([]registeredSecret(nil), values...),
	}
	for index, secret := range m.values {
		state := 0
		for _, c := range secret.value {
			next, ok := m.nodes[state].next[c]
			if !ok {
				next = len(m.nodes)
				m.nodes[state].next[c] = next
				m.nodes = append(m.nodes, matcherNode{next: make(map[byte]int)})
			}
			state = next
		}
		m.nodes[state].out = append(m.nodes[state].out, index)
		if len(secret.value) > m.maxLen {
			m.maxLen = len(secret.value)
		}
	}

	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		queue = append(queue, child)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for c, child := range m.nodes[state].next {
			queue = append(queue, child)
			failure := m.nodes[state].fail
			for failure != 0 {
				if _, ok := m.nodes[failure].next[c]; ok {
					break
				}
				failure = m.nodes[failure].fail
			}
			if next, ok := m.nodes[failure].next[c]; ok && next != child {
				m.nodes[child].fail = next
			}
			failedOutputs := m.nodes[m.nodes[child].fail].out
			m.nodes[child].out = append(m.nodes[child].out, failedOutputs...)
		}
	}
	return m
}

func (m *valueMatcher) addCandidates(b []byte, best map[int]matchCandidate) {
	state := 0
	for offset, c := range b {
		for state != 0 {
			if _, ok := m.nodes[state].next[c]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[c]; ok {
			state = next
		}
		for _, index := range m.nodes[state].out {
			secret := m.values[index]
			start := offset + 1 - len(secret.value)
			setCandidate(best, start, matchCandidate{
				end:         offset + 1,
				replacement: secret.replacement,
				exact:       true,
			})
		}
	}
}

func detectCredentialShapes(b []byte) []ShapeMatch {
	var matches []ShapeMatch
	for i := 0; i < len(b); {
		var kind string
		var end int
		switch {
		case bytes.HasPrefix(b[i:], []byte("sk-")):
			end = tokenEnd(b, i+3, i)
			if end-(i+3) >= 16 {
				kind = "openai-key"
			}
		case bytes.HasPrefix(b[i:], []byte("ghp_")):
			end = tokenEnd(b, i+4, i)
			if end-(i+4) >= 36 {
				kind = "github-token"
			}
		case bytes.HasPrefix(b[i:], []byte("AKIA")):
			end = i + 20
			if end <= len(b) && allAWSKeyChars(b[i+4:end]) {
				kind = "aws-access-key"
			}
		case bytes.HasPrefix(b[i:], []byte("-----BEGIN ")):
			end = privateKeyHeaderEnd(b, i)
			if end > i {
				kind = "private-key"
			}
		}
		if kind == "" {
			i++
			continue
		}
		matches = append(matches, ShapeMatch{Kind: kind, Offset: i, Length: end - i})
		i = end
	}
	return matches
}

func tokenEnd(b []byte, start, matchStart int) int {
	limit := matchStart + maxShapeMatchLen
	if limit > len(b) {
		limit = len(b)
	}
	end := start
	for end < limit && credentialTokenByte(b[end]) {
		end++
	}
	return end
}

func privateKeyHeaderEnd(b []byte, start int) int {
	const prefix = "-----BEGIN "
	const suffix = "PRIVATE KEY-----"

	labelStart := start + len(prefix)
	limit := start + maxShapeMatchLen
	if limit > len(b) {
		limit = len(b)
	}
	for i := labelStart; i+len(suffix) <= limit; i++ {
		if b[i] == '\n' || b[i] == '\r' {
			return 0
		}
		if bytes.HasPrefix(b[i:limit], []byte(suffix)) {
			label := b[labelStart:i]
			if len(label) > 0 && label[len(label)-1] != ' ' {
				return 0
			}
			if !validPrivateKeyLabel(label) {
				return 0
			}
			return i + len(suffix)
		}
	}
	return 0
}

func validPrivateKeyLabel(label []byte) bool {
	for _, c := range label {
		if (c < 'A' || c > 'Z') && c != ' ' {
			return false
		}
	}
	return true
}

func allAWSKeyChars(b []byte) bool {
	for _, c := range b {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func credentialTokenByte(c byte) bool {
	return asciiAlphaNum(c) || c == '_' || c == '-'
}

func asciiAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
