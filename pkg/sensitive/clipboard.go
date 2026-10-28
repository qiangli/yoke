// Package sensitive contains shape checks shared by the human ask form and
// browser guard. These checks are intentionally conservative: an uncertain
// clipboard value is entered through the form instead.
package sensitive

import (
	"regexp"
	"strings"
)

// ClipboardCategory classifies a guarded field for clipboard shape checks.
func ClipboardCategory(name, kind string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "social security"), strings.Contains(n, "ssn"):
		return "ssn"
	case strings.Contains(n, "itin"):
		return "itin"
	case strings.Contains(n, "ein"), strings.Contains(n, "employer identification"):
		return "ein"
	case strings.Contains(n, "routing"):
		return "routing"
	case strings.Contains(n, "iban"):
		return "iban"
	case strings.Contains(n, "card"), strings.Contains(n, "credit"):
		return "card"
	case strings.Contains(n, "account number"):
		return "account"
	case strings.Contains(n, "one-time"), strings.Contains(n, "otp"), strings.Contains(n, "verification code"):
		return "otp"
	case strings.Contains(n, "password"), strings.Contains(n, "passcode"):
		return "password"
	case strings.Contains(n, "username"), strings.Contains(n, "user name"):
		return "username"
	case strings.Contains(n, "id"):
		return "id"
	case kind == "secret":
		return "secret"
	default:
		return "unknown"
	}
}

// ClipboardShape checks only the expected syntax. It never returns the value.
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
	return true, ""
}

// ClipboardPattern is the portable regex used by native dialogs for their
// disabled-button preview. The Go check is repeated after the click.
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
		"password": `^[\s\S]{1,1024}$`,
		"secret":   `^[\s\S]{1,1024}$`,
	}
	pattern, ok := patterns[category]
	return pattern, ok
}
