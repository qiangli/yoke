// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package audit

import (
	"os"
	"regexp"
	"strings"
)

// ActorFromEnv resolves the accountable identity from the ambient environment
// the launcher set. bashy's shim launcher injects BASHY_PRINCIPAL
// (dhnt:agent/<nick>), BASHY_AGENT_TOOL/_NAME, and BASHY_SESSION when an agent
// drives the shell; a human at an interactive prompt has only the uid/pid,
// which is still a valid actor.
func ActorFromEnv() Actor {
	a := Actor{UID: os.Getuid(), PID: os.Getpid()}
	a.Human = firstEnv("BASHY_PRINCIPAL", "USER", "LOGNAME")
	a.Agent = firstEnv("BASHY_AGENT_TOOL", "BASHY_AGENT_NAME", "BASHY_AGENT")
	a.Model = os.Getenv("BASHY_MODEL")
	a.Session = firstEnv("BASHY_SESSION", "BASHY_EPISODE")
	return a
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// secretishKey matches env-var / flag names that name a credential, so a
// NAME=VALUE or --token VALUE argument has its VALUE masked in the record.
var secretishKey = regexp.MustCompile(`(?i)(secret|token|passwd|password|api[-_]?key|access[-_]?key|private[-_]?key|credential|auth)`)

// highEntropyToken matches a bare argument that looks like a credential by
// shape — long, no spaces, mixed alphanumeric — even when no key names it.
var highEntropyToken = regexp.MustCompile(`^[A-Za-z0-9_\-./+=]{24,}$`)

// Redact masks secret-looking values in an argv before it is written to the
// log, returning the cleaned argv and the count masked. This is the minimal,
// self-contained pass so the audit log is not itself a secret-leak; the
// gitleaks-grade streaming redactor (design item 1d) supersedes it. It masks:
//   - the VALUE in a NAME=VALUE token whose NAME looks like a credential;
//   - the argument AFTER a credential-looking flag (--token X, -p X);
//   - a bare token that looks high-entropy AND sits next to a credential flag.
//
// It deliberately does NOT mask every long string (paths, hashes, URLs are
// legitimately long), only those a key or flag marks as a secret — false
// masking would gut the audit trail's usefulness.
func Redact(argv []string) (out []string, masked int) {
	out = make([]string, len(argv))
	copy(out, argv)
	for i, a := range out {
		if k, v, ok := strings.Cut(a, "="); ok && v != "" && secretishKey.MatchString(k) {
			out[i] = k + "=" + mask
			masked++
			continue
		}
		if i > 0 && looksLikeCredFlag(out[i-1]) && (highEntropyToken.MatchString(a) || len(a) >= 8) {
			out[i] = mask
			masked++
		}
	}
	return out, masked
}

const mask = "‹redacted›"

func looksLikeCredFlag(s string) bool {
	if !strings.HasPrefix(s, "-") {
		return false
	}
	return secretishKey.MatchString(strings.TrimLeft(s, "-")) ||
		s == "-p" || s == "-P" // common password short flags
}
