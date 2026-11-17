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
	"time"
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
	return writeAtomic0600(p, value)
}

func writeAtomic0600(p, value string) error {
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

// onceTTL bounds how long a not-remembered answer waits to be used.
const onceTTL = 15 * time.Minute

func sheetOncePath(name string) (string, error) {
	if !ValidSheetSecretName(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	return filepath.Join(SheetSecretDir(), ".once", name), nil
}

// PutSheetOnce holds a value the human entered without Remember: a one-use
// 0600 sink under name, consumed by the first TakeSheetSecret and dropped
// after onceTTL.
func PutSheetOnce(name, value string) error {
	p, err := sheetOncePath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeAtomic0600(p, value)
}

// SheetSecretStored reports whether name has a value: remembered, or held
// once and not yet expired.
func SheetSecretStored(name string) bool {
	if p, err := sheetSecretPath(name); err == nil {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if p, err := sheetOncePath(name); err == nil {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < onceTTL {
			return true
		}
	}
	return false
}

// TakeSheetSecret returns name's value for a fill by reference: a one-use
// answer first (consumed: removed before the value is returned, so it is
// taken at most once), else the remembered value.
func TakeSheetSecret(name string) (string, error) {
	p, err := sheetOncePath(name)
	if err != nil {
		return "", err
	}
	claimed := p + fmt.Sprintf(".taken-%d", time.Now().UnixNano())
	if err := os.Rename(p, claimed); err == nil {
		b, rerr := os.ReadFile(claimed)
		fi, serr := os.Stat(claimed)
		os.Remove(claimed)
		if rerr == nil && serr == nil && time.Since(fi.ModTime()) < onceTTL {
			return string(b), nil
		}
	}
	return GetSheetSecret(name)
}

// sheetSitePath is where an entry records the site it was made for (S529.9):
// a sibling directory, so no secret name can collide with it.
func sheetSitePath(name string) (string, error) {
	if !ValidSheetSecretName(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	return filepath.Join(SheetSecretDir(), ".site", name), nil
}

// SetSheetSecretSite records the site (host) an entry was made for; "" clears
// it, and an entry without a site cannot fill a page by reference.
func SetSheetSecretSite(name, site string) error {
	p, err := sheetSitePath(name)
	if err != nil {
		return err
	}
	if site == "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeAtomic0600(p, site)
}

// SheetSecretSite is the site an entry was made for ("" when none).
func SheetSecretSite(name string) string {
	p, err := sheetSitePath(name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// DeleteSheetSecret removes name (remembered and held once); a missing name
// is not an error.
func DeleteSheetSecret(name string) error {
	p, err := sheetSecretPath(name)
	if err != nil {
		return err
	}
	o, _ := sheetOncePath(name)
	st, _ := sheetSitePath(name)
	for _, f := range []string{p, o, st} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
