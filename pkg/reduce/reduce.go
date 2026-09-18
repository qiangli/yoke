// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/admission"
)

const (
	// DefaultBudgetBytes is the inherited 40 KiB conversational ceiling
	// (bashy-coordination-context-efficiency-plan.md §4.2). It costs nothing when
	// output is small and bounds the catastrophic tail.
	DefaultBudgetBytes = 40 * 1024

	// DefaultRecoverVerb is the runnable recovery command prefix. The handle must
	// be a command, not an opaque id, so the agent needs no new tool to restore
	// the elided region and can compose it (`bashy out 9c2d4f1a | rg FAIL`).
	DefaultRecoverVerb = "bashy out"

	// DefaultKeepHint is the shell-path prevention command. An agent that has to
	// search for how to opt out will just re-run the command, so the marker
	// carries it inline.
	DefaultKeepHint = "BASHY_OUTPUT_REDUCE=off"

	// digestDisplayHex is how many hex characters of the digest the human-facing
	// marker shows before an ellipsis; it mirrors the contract's example
	// (`sha256:9c2d…`). The runnable Handle, not this display, carries recovery.
	digestDisplayHex = 4
)

// Config controls one reduction.
type Config struct {
	// BudgetBytes is the byte ceiling for the reduced view, marker included.
	// Zero selects DefaultBudgetBytes. Byte budgets are the correctness gate;
	// token counts are analysis only and never on this path (contract §2.4).
	BudgetBytes int

	// TelemetryHintsOnly enables the always-safe Stage 0.1 view without enabling
	// general head elision. If no duplicate classified hints exist, output is
	// returned complete even when it exceeds BudgetBytes.
	TelemetryHintsOnly bool

	// HomeDir, when non-empty, is canonicalized to $HOME before redaction,
	// duplicate comparison, spilling, or view construction.
	HomeDir string

	// Redactor, when non-nil, masks the complete bytes BEFORE they are spilled,
	// so the retrievable artifact inherits the secret gate (contract §2.7). The
	// host is responsible for wiring it; this package cannot enforce it.
	Redactor Redactor

	// RecoverVerb and KeepHint override the marker's runnable commands. Empty
	// selects the defaults above.
	RecoverVerb string
	KeepHint    string
}

// Result is a reduced view plus the accounting a caller reports to telemetry.
type Result struct {
	// Text is the bytes to emit in place of the original. When Reduced is false
	// it is the (possibly redacted) input unchanged and no spill happened.
	Text string

	Reduced bool // true when a region was elided and spilled
	Binary  bool // true when the input was not valid UTF-8 (detected, not repaired)

	Digest string // "sha256:<hex>" of the spilled bytes; empty when not Reduced
	Handle string // runnable digest-prefix handle; empty when not Reduced
	Marker string // the elision marker line; empty when not Reduced

	FullBytes    int // length of the complete canonicalized/redacted output
	KeptBytes    int // bytes retained inline in Text
	OmittedBytes int // bytes elided behind the handle
	OmittedLines int // newline-delimited lines elided behind the handle

	// SuppressedHints is the number of later, byte-exact telemetry hint lines
	// represented by the single recovery annotation.
	SuppressedHints int
	SuppressedBytes int
}

// Reduce enforces the byte ceiling on full and, when it must elide, writes the
// complete (redacted) bytes to a content-addressed spill BEFORE producing the
// reduced view — so the context is lossy while the system is not, and the model
// holds the decision to restore. store may be nil only when no reduction is
// needed.
//
// Command output is routinely not valid UTF-8 (binary, control bytes, a partial
// multibyte rune at a chunk boundary). admission.UTF8Prefix refuses invalid
// UTF-8 by design, so this takes an explicit binary path: detect it, do not
// repair it, and represent it as a header plus handle rather than inlining raw
// bytes into agent context.
func Reduce(store *Store, full []byte, cfg Config) (Result, error) {
	budget := cfg.BudgetBytes
	if budget <= 0 {
		budget = DefaultBudgetBytes
	}
	verb := strings.TrimSpace(cfg.RecoverVerb)
	if verb == "" {
		verb = DefaultRecoverVerb
	}
	keep := strings.TrimSpace(cfg.KeepHint)
	if keep == "" {
		keep = DefaultKeepHint
	}

	body := CanonicalizeHome(full, cfg.HomeDir)
	if cfg.Redactor != nil {
		body = cfg.Redactor.Redact(body)
	}

	res := Result{FullBytes: len(body)}
	binary := !utf8.Valid(body)
	view := body
	if !binary {
		view, res.SuppressedHints, res.SuppressedBytes = deduplicateTelemetryHints(body)
	}
	if cfg.TelemetryHintsOnly && (binary || res.SuppressedHints == 0) {
		res.Text = string(body)
		res.KeptBytes = len(body)
		return res, nil
	}

	// Fast path: valid text that already fits. Nothing is elided, so — per the
	// §2.1 corollary — no marker is emitted, because there is no region an agent
	// could mistake a partial view for.
	if !binary && res.SuppressedHints == 0 && len(body) <= budget {
		res.Text = string(body)
		res.KeptBytes = len(body)
		return res, nil
	}

	// From here a region is elided, so the complete bytes must be spilled first.
	if store == nil {
		return Result{}, errors.New("reduce: reduction required but no spill store was provided")
	}
	digest, err := store.Put(body)
	if err != nil {
		return Result{}, fmt.Errorf("reduce: spill failed before emit: %w", err)
	}
	res.Reduced = true
	res.Binary = binary
	res.Digest = digest
	res.Handle = store.shortestHandle(strings.TrimPrefix(digest, digestPrefix))
	if cfg.TelemetryHintsOnly {
		res.KeptBytes = len(view)
		res.OmittedBytes = res.SuppressedBytes
		res.OmittedLines = countLines(body) - countLines(view)
		res.Marker = buildMarker(res, verb, keep)
		res.Text = joinMarker(string(view), res.Marker)
		if len(res.Text) < budget {
			res.Text += "\n"
		}
		if len(res.Text) > budget {
			return Result{}, fmt.Errorf("reduce: telemetry view requires %d bytes, budget is %d", len(res.Text), budget)
		}
		return res, nil
	}

	if binary {
		// detect, do not repair: never inline raw binary into agent context.
		res.OmittedBytes = len(body)
		res.OmittedLines = 0
		res.Marker = buildMarker(res, verb, keep)
		if len(res.Marker) > budget {
			return Result{}, fmt.Errorf("reduce: recovery marker requires %d bytes, budget is %d", len(res.Marker), budget)
		}
		res.Text = res.Marker
		if len(res.Marker) < budget {
			res.Text += "\n"
		}
		return res, nil
	}

	// Text path: keep a valid-UTF-8 head within the budget the marker leaves, and
	// place the marker at the elision site. The marker length depends on the
	// omitted counts, which depend on the head length, so converge by fixpoint —
	// a couple of iterations, since only decimal digit counts can move.
	totalLines := countLines(body)
	viewLines := countLines(view)
	res.KeptBytes = 0
	res.OmittedBytes = len(body)
	res.OmittedLines = totalLines
	res.Marker = buildMarker(res, verb, keep)
	if len(res.Marker) > budget {
		return Result{}, fmt.Errorf("reduce: recovery marker requires %d bytes, budget is %d", len(res.Marker), budget)
	}
	// Start with the entire Stage 0.1 view. If its mandatory annotation pushes
	// the result over budget, converge on a UTF-8-safe prefix.
	head := string(view)
	for range len(view) + 2 {
		headMax := len(head)
		h, perr := admission.UTF8Prefix(string(view), headMax)
		if perr != nil {
			// body validated above, so this cannot fail; treat defensively.
			return Result{}, perr
		}
		res.KeptBytes = len(h)
		res.OmittedBytes = len(body) - len(h)
		res.OmittedLines = totalLines - countLines([]byte(h))
		res.Marker = buildMarker(res, verb, keep)
		if len(res.Marker) > budget {
			// The minimum marker was checked above. Discard this candidate head;
			// its changed counts made the marker larger than the available budget.
			head = ""
			res.KeptBytes = 0
			res.OmittedBytes = len(body)
			res.OmittedLines = totalLines
			res.Marker = buildMarker(res, verb, keep)
			break
		}
		separator := markerSeparatorBytes(h)
		trailing := 0
		if len(h)+separator+len(res.Marker) < budget {
			trailing = 1
		}
		if len(h)+separator+len(res.Marker)+trailing <= budget {
			head = h
			break
		}
		nextMax := budget - len(res.Marker) - separator - trailing
		if nextMax < 0 {
			head = ""
			res.KeptBytes = 0
			res.OmittedBytes = len(body)
			res.OmittedLines = totalLines
			res.Marker = buildMarker(res, verb, keep)
			break
		}
		next, nerr := admission.UTF8Prefix(string(view), nextMax)
		if nerr != nil {
			return Result{}, nerr
		}
		if len(next) == len(h) {
			head = next
			break
		}
		head = next
	}
	res.KeptBytes = len(head)
	res.OmittedBytes = len(body) - len(head)
	res.OmittedLines = totalLines - countLines([]byte(head))
	// If only duplicates were removed, the accounting above is equivalent to
	// suppressedBytes/viewLines; keep the explicit values live as invariants.
	if len(head) == len(view) {
		res.OmittedBytes = res.SuppressedBytes
		res.OmittedLines = totalLines - viewLines
	}
	res.Marker = buildMarker(res, verb, keep)
	if len(res.Marker) > budget {
		return Result{}, fmt.Errorf("reduce: recovery marker requires %d bytes, budget is %d", len(res.Marker), budget)
	}
	res.Text = joinMarker(head, res.Marker)
	if len(res.Text) < budget {
		res.Text += "\n"
	}
	if len(res.Text) > budget {
		return Result{}, fmt.Errorf("reduce: reduced view requires %d bytes, budget is %d", len(res.Text), budget)
	}
	return res, nil
}

// buildMarker renders the inline elision marker. It carries the omitted count,
// the digest, the RUNNABLE recovery command, and — for the shell path — the
// prevention command, all in a single byte-budgeted line.
func buildMarker(r Result, verb, keep string) string {
	var parts []string
	if r.SuppressedHints > 0 {
		parts = append(parts, fmt.Sprintf("%s duplicate telemetry hints suppressed", commaInt(r.SuppressedHints)))
	}
	if r.Binary {
		parts = append(parts, humanBytes(r.OmittedBytes)+" binary output elided")
	} else if r.SuppressedHints == 0 || r.OmittedBytes > r.SuppressedBytes {
		parts = append(parts, fmt.Sprintf("%s lines / %s elided", commaInt(r.OmittedLines), humanBytes(r.OmittedBytes)))
	}
	return fmt.Sprintf("[bashy: %s · %s · full: %s %s · keep: %s]",
		strings.Join(parts, " · "), shortDigest(r.Digest), verb, r.Handle, keep)
}

func markerSeparatorBytes(head string) int {
	if head == "" || strings.HasSuffix(head, "\n") {
		return 0
	}
	return 1
}

func joinMarker(head, marker string) string {
	if head == "" {
		return marker
	}
	if strings.HasSuffix(head, "\n") {
		return head + marker
	}
	return head + "\n" + marker
}

func shortDigest(digest string) string {
	hexsum := strings.TrimPrefix(digest, digestPrefix)
	n := min(digestDisplayHex, len(hexsum))
	return digestPrefix + hexsum[:n] + "…"
}

// countLines counts newline-delimited lines. A trailing byte without a final
// newline still counts as a line; empty input is zero lines.
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.0f %s", float64(n)/float64(div), units[exp])
}

// commaInt renders n with thousands separators, matching the contract example
// ("4,812 lines").
func commaInt(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
