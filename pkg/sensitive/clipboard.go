// Package sensitive contains shape checks shared by the human ask form and
// browser guard. These checks are intentionally conservative: an uncertain
// clipboard value is entered through the form instead.
package sensitive

import (
	"regexp"
	"strings"
)

// ClipboardCategory classifies a guarded field for clipboard shape checks.
// Terms match on word boundaries, so a prompt that merely contains "being"
// or "provide" is not taken for an EIN or an ID field.
func ClipboardCategory(name, kind string) string {
	n := strings.ToLower(name)
	for _, c := range categoryWords {
		if c.re.MatchString(n) {
			return c.category
		}
	}
	if kind == "secret" {
		return "secret"
	}
	return "unknown"
}

// categoryWords is ClipboardCategory's table, first match wins.
var categoryWords = []struct {
	category string
	re       *regexp.Regexp
}{
	{"ssn", regexp.MustCompile(`\bsocial security\b|\bssn\b`)},
	{"itin", regexp.MustCompile(`\bitin\b`)},
	{"ein", regexp.MustCompile(`\bein\b|\bemployer identification\b`)},
	{"routing", regexp.MustCompile(`\brouting\b`)},
	{"iban", regexp.MustCompile(`\biban\b`)},
	{"card", regexp.MustCompile(`\bcard\b|\bcredit\b`)},
	{"account", regexp.MustCompile(`\baccount number\b`)},
	{"otp", regexp.MustCompile(`\bone-time\b|\botp\b|\bverification code\b`)},
	{"password", regexp.MustCompile(`\bpassword\b|\bpasscode\b`)},
	{"username", regexp.MustCompile(`\busername\b|\buser name\b`)},
	{"id", regexp.MustCompile(`\bid\b`)},
}

// ClipboardShape is the AUTHORITATIVE check of a clipboard value against a
// field category: the syntax (ClipboardPattern) and then, where the category
// has one, the same validator the browser guard's value check uses — ABA
// 3-7-1 checksum, Luhn + issuer, EIN campus prefix, SSN/ITIN group rules,
// IBAN mod-97. It never returns the value.
func ClipboardShape(category, value string) (bool, string) {
	if value == "" {
		return false, "clipboard is empty"
	}
	pattern, ok := ClipboardPattern(category)
	if !ok {
		return false, "clipboard shape is uncertain"
	}
	if !regexp.MustCompile(pattern).MatchString(value) {
		return false, "clipboard doesn't look like a " + category
	}
	if check := clipboardChecks[category]; check != nil && !check(value) {
		return false, "clipboard fails the " + category + " check"
	}
	return true, ""
}

// clipboardChecks are the checksum/assignment rules ClipboardShape applies
// after the syntax matched.
var clipboardChecks = map[string]func(string) bool{
	"ssn":     ValidSSN,
	"itin":    ValidITIN,
	"ein":     func(v string) bool { return ValidEIN(v) || (len(v) == 9 && ValidEIN(v[:2]+"-"+v[2:])) },
	"routing": ValidABA,
	"iban":    func(v string) bool { return ValidIBAN(v) },
	"card":    ValidCard,
}

// ClipboardPattern is the portable regex used by native dialogs for their
// disabled-button preview. It is only a PRE-FILTER that may disable the
// button: a value it rejects is rejected by ClipboardShape too, but one it
// accepts can still fail ClipboardShape's checksum after the click.
func ClipboardPattern(category string) (string, bool) {
	patterns := map[string]string{
		"ssn":      `^\d{3}-?\d{2}-?\d{4}$`,
		"itin":     `^9\d{2}-?[7-9]\d-?\d{4}$`,
		"ein":      `^\d{2}-?\d{7}$`,
		"routing":  `^\d{9}$`,
		"iban":     `^[A-Za-z]{2}\d{2}[A-Za-z0-9]{11,30}$`,
		"card":     `^(?:\d[ -]?){12,18}\d$`,
		"account":  `^\d{4,17}$`,
		"otp":      `^[A-Za-z0-9]{4,10}$`,
		"username": `^[^\s]{1,128}$`,
		"id":       `^[A-Za-z0-9][A-Za-z0-9 _-]{0,127}$`,
		// Go's regexp caps a repeat count at 1000.
		"password": `^[\s\S]{1,1000}$`,
		"secret":   `^[\s\S]{1,1000}$`,
	}
	pattern, ok := patterns[category]
	return pattern, ok
}
