// Redaction for gws diagnostics (Sprint 457 story f25d7aa4437c).
//
// Credentials, auth headers, signed URLs, sensitive query parameters,
// token-bearing page tokens, and configured sensitive payload fields are
// redacted before stdout/stderr diagnostics, errors, previews, logs,
// tracing, or MCP exposure. Debug mode never weakens redaction. Redaction
// is validated with canary secrets, including provider error bodies and
// subprocess failures.
//
// Two layers, both existing machinery, no new redactor:
//   - argv: policy/audit.Redact (credential-looking NAME=VALUE and flag values)
//   - text: literal masking of declared secret values (credential file
//     contents the caller registers, canary or real)
package gws

import (
	"strings"

	"github.com/qiangli/yoke/pkg/policy/audit"
)

// Redacted is the placeholder that replaces secret material.
const Redacted = "‹redacted›"

// RedactArgs masks secret-looking values in an argv (audit.Redact) and
// additionally masks every declared secret literally, wherever it appears.
// It returns the cleaned argv and the number of values masked.
func RedactArgs(argv []string, secrets ...string) ([]string, int) {
	out, masked := audit.Redact(argv)
	for i, a := range out {
		if c, changed := maskLiterals(a, secrets); changed {
			out[i] = c
			masked++
		}
	}
	return out, masked
}

// RedactText masks every declared secret literally in free text (provider
// error bodies, subprocess output, previews, logs). An empty secret list
// masks nothing; empty secret values are skipped so no text is shredded.
func RedactText(text string, secrets ...string) string {
	out, _ := maskLiterals(text, secrets)
	return out
}

// maskLiterals replaces each non-empty secret with the placeholder.
func maskLiterals(text string, secrets []string) (string, bool) {
	changed := false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(text, s) {
			text = strings.ReplaceAll(text, s, Redacted)
			changed = true
		}
	}
	return text, changed
}
