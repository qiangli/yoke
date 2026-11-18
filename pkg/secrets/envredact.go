package secrets

import (
	"os"
	"sort"
	"strings"
)

// EnvValueRedactor combines registered app credentials with vault-projected
// values present in environ and masks credential-shaped key/value forms.
// It never fetches or renders vault contents. Vault names are registered in
// sorted order; app registrations remain live through the shared registry.
func EnvValueRedactor(environ []string) *Redactor {
	redactor := NewRedactor()
	redactor.host = HostRedactor()
	_ = redactor.SetShapeMode(ShapeMask)
	names := VaultEnvNames()
	if len(names) == 0 {
		return redactor
	}
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		value, ok := values[name]
		if !ok {
			continue
		}
		// A value too short to redact safely is skipped: MinRedactedValueLen
		// exists because masking short strings corrupts ordinary output.
		_ = redactor.Register(name, value)
	}
	return redactor
}

// RedactKnownValues masks host and vault values plus credential-shaped forms
// in one string, using the same policy as log and agent-output writers.
func RedactKnownValues(s string) string {
	return string(EnvValueRedactor(os.Environ()).Redact([]byte(s)))
}
