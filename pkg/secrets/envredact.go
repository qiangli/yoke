package secrets

import (
	"os"
	"sort"
	"strings"
)

// EnvValueRedactor returns a Redactor with every vault-projected value that is
// present in environ registered under its local name. It is the one
// construction every member-facing egress point needs: the vault names the
// secret-carrying variables (VaultEnvNames), the environment supplies the
// rendered values, and the returned Redactor masks exactly those values.
//
// It never fetches or renders the vault itself: a value that is not already in
// the calling process's environment is not registered (masking it there is
// impossible anyway — the value never reached this process). Names are
// registered in sorted order so diagnostics that surface registration order
// stay deterministic.
func EnvValueRedactor(environ []string) *Redactor {
	redactor := NewRedactor()
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

// RedactKnownValues masks the vault-projected values present in the process
// environment inside s. It is the convenience form of EnvValueRedactor for
// egress points that redact one string and do not need the Redactor itself.
func RedactKnownValues(s string) string {
	return string(EnvValueRedactor(os.Environ()).Redact([]byte(s)))
}
