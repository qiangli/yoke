package live

// The local-only sheet secret store (Sprint 529, S529.7/S529.8): a copy-sheet
// field may name a secret, and the hub reads its value from here when the
// human clicks Reveal or Copy. One 0600 file per name under a 0700 directory
// in $BASHY_HOME/browser — never synced, never the cloudbox vault. Threat
// model: docs/browser-sheet-secrets.md.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// sheetSecretName is the allowed shape of a secret name: it is a file name,
// so no separators, no dot-only names, nothing a path could escape through.
var sheetSecretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrNoSheetSecret reports a name with no stored value.
var ErrNoSheetSecret = errors.New("no stored value")

// ValidSheetSecretName reports whether name may name a sheet secret.
func ValidSheetSecretName(name string) bool { return sheetSecretName.MatchString(name) }

// SheetSecretDir is the store directory.
func SheetSecretDir() string { return filepath.Join(gateDir(), "sheet-secrets") }

func sheetSecretPath(name string) (string, error) {
	if !ValidSheetSecretName(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	return filepath.Join(SheetSecretDir(), name), nil
}

// GetSheetSecret returns the stored value of name, or ErrNoSheetSecret.
func GetSheetSecret(name string) (string, error) {
	p, err := sheetSecretPath(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoSheetSecret
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// PutSheetSecret stores value under name (0600, written atomically).
func PutSheetSecret(name, value string) error {
	p, err := sheetSecretPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// DeleteSheetSecret removes name; a missing name is not an error.
func DeleteSheetSecret(name string) error {
	p, err := sheetSecretPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
