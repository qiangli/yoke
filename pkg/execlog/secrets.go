// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"regexp"
	"strings"
)

// maskSecrets replaces credential VALUES before anything else looks at argv.
//
// This runs first, ahead of the identity scrubber, and the ordering is not
// cosmetic: identity tagging rewrites the hostname inside
// `psql postgres://u:p@host/db`, after which the URL no longer matches the
// credential pattern and the password is written to disk.
//
// It is deliberately broader than the audit log's pass, which recognises only
// `NAME=VALUE` and the following word. Three shapes it misses are all common:
//
//	-pSECRET                      attached to a short flag, one word
//	-H "Authorization: Bearer …"  the secret is inside an unrelated flag's value
//	https://user:pass@host/       the secret is inside a URL
//
// A miss here is permanent — the record is on disk before anyone reads it — so
// this errs toward masking. A masked value that was not a secret costs one
// fragmented template; an unmasked one that was costs a credential.
const secretMask = "<SECRET>"

func maskSecrets(argv []string) []string {
	out := make([]string, len(argv))
	copy(out, argv)

	for i := 0; i < len(out); i++ {
		a := out[i]

		// NAME=VALUE, whether an env assignment or a --flag=value.
		if name, val, ok := strings.Cut(a, "="); ok && val != "" {
			if secretishName(name) {
				out[i] = name + "=" + secretMask
				continue
			}
		}

		// -pSECRET: attached to a short flag, so a next-word check never sees
		// it. Only the ATTACHED form qualifies — see shortSecretFlag.
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && shortSecretFlag(a[:2]) {
			out[i] = a[:2] + secretMask
			continue
		}

		// The value in the NEXT word.
		if secretishName(a) && i+1 < len(out) {
			out[i+1] = secretMask
			i++
			continue
		}

		out[i] = maskInline(a)
	}
	return out
}

// maskInline catches a secret embedded in an otherwise ordinary word.
func maskInline(s string) string {
	if s == "" {
		return s
	}
	s = credURLRE.ReplaceAllString(s, "$1"+secretMask+"@")
	s = bearerRE.ReplaceAllString(s, "${1}"+secretMask)
	s = tokenShapeRE.ReplaceAllString(s, secretMask)
	return s
}

// secretishName reports a flag or variable name that introduces a credential.
//
// Fragments only. A short flag is NOT secretish by this test, because the
// short forms are ambiguous across tools and this predicate is what decides
// whether to swallow the following word.
func secretishName(name string) bool {
	n := strings.ToLower(strings.TrimLeft(name, "-"))
	if n == "" {
		return false
	}
	for _, frag := range secretFragments {
		if strings.Contains(n, frag) {
			return true
		}
	}
	return false
}

// shortSecretFlag reports a short flag that carries a password when a value is
// ATTACHED to it.
//
// The attached form is the whole test, and it is what keeps `ssh -p 2222 host`
// working: `-p` as a separate word is a port and is classified <PORT>, while
// `-psecret` as one word is mysql's password and is masked. Both spellings are
// what the real tools use, so form distinguishes them without a per-binary
// table that would have to know every program on the machine.
func shortSecretFlag(flag string) bool {
	switch flag {
	case "-p", "-w":
		return true
	}
	return false
}

var secretFragments = []string{
	"passwd", "password", "secret", "token", "apikey", "api_key",
	"credential", "auth", "bearer", "session", "cookie", "privatekey",
	"private_key", "accesskey", "access_key", "signature",
}

// maskForeignHomes abstracts a home directory belonging to somebody else.
//
// The scrubber is seeded from THIS host, so it masks this user's home and
// nothing more. That is right for its own purpose and wrong here: a store that
// records every command sees other accounts' paths constantly — in a container
// mount, a copied script, another user's checkout — and each one is identity.
//
// The username is what gets replaced, not the whole path, because the tail
// ("/.ssh/id_rsa", "/projects/x") is what makes the record useful evidence.
func maskForeignHomes(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = homePathRE.ReplaceAllString(a, "${1}${2}<USER>/")
		out[i] = winHomePathRE.ReplaceAllString(out[i], `${1}<USER>\`)
	}
	return out
}

// preclassifyHostShapes reduces `user@host` and `user@host:port` to classes
// before the identity scrubber can mistake them for an email address.
//
// It runs only on the TEMPLATE's copy of argv, never on the stored evidence:
// the record should still say which host was reached (under a co-reference
// tag), while the shared node key should say only that A host was reached.
func preclassifyHostShapes(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = userHostRE.ReplaceAllStringFunc(a, func(m string) string {
			if _, rest, ok := strings.Cut(m, "@"); ok {
				if _, port, hasPort := strings.Cut(rest, ":"); hasPort && port != "" {
					return "<USER>@<HOST>:<PORT>"
				}
			}
			return "<USER>@<HOST>"
		})
	}
	return out
}

// detag collapses redact's co-reference tags to bare classes.
//
// `‹host:af34›` is stable and non-reversible, but the hash is derived from the
// value, so two machines produce two different tags for the same command. The
// class is identical everywhere, which is what makes templates comparable
// across hosts without any of them exporting a hostname.
func detag(s string) string {
	return tagRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := tagRE.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		switch sub[1] {
		case "path":
			return "<HOMEPATH>"
		case "host":
			return "<HOST>"
		case "user":
			return "<USER>"
		case "ip":
			return "<IP>"
		case "mac":
			return "<MAC>"
		case "email":
			return "<EMAIL>"
		}
		return "<" + strings.ToUpper(sub[1]) + ">"
	})
}

var (
	// A URL carrying inline credentials: scheme://user:pass@host
	credURLRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]+:)[^@/\s]+@`)

	// Someone else's home directory, in the three shapes the platforms use.
	homePathRE    = regexp.MustCompile(`(^|[\s=:"'])(/Users/|/home/)[^/\s"']+/`)
	winHomePathRE = regexp.MustCompile(`(?i)([A-Z]:\\Users\\)[^\\\s"']+\\`)

	// redact's tag rendering: ‹kind:hash›
	tagRE = regexp.MustCompile(`‹([a-z]+):[0-9a-f]+›`)

	// user@host, optionally with a port. Anchored on word edges so it does not
	// eat the user half of a credential URL that pass 1 already masked.
	userHostRE = regexp.MustCompile(`\b[A-Za-z0-9._-]+@[A-Za-z0-9.-]+(?::\d+)?\b`)

	// An Authorization-style header value.
	bearerRE = regexp.MustCompile(`(?i)((?:bearer|token|basic)\s+)[A-Za-z0-9._~+/=-]{8,}`)

	// Vendor-prefixed keys and JWTs. These are recognisable on shape alone, so
	// they are caught even when the flag that carried them looks innocent.
	tokenShapeRE = regexp.MustCompile(
		`\b(?:sk-[A-Za-z0-9_-]{16,}` +
			`|ghp_[A-Za-z0-9]{20,}` +
			`|gho_[A-Za-z0-9]{20,}` +
			`|github_pat_[A-Za-z0-9_]{20,}` +
			`|xox[abprs]-[A-Za-z0-9-]{10,}` +
			`|AKIA[0-9A-Z]{16}` +
			`|AIza[0-9A-Za-z_-]{30,}` +
			`|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`)
)
