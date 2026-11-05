package src

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// ErrStaleBase means another operation changed the principal's active base.
var ErrStaleBase = errors.New("src: active revision changed")

// lockActivation serializes all public activation/restore/adoption operations
// for this app/principal, including their version-line and state-record writes.
// Private *Unlocked helpers may be composed only while this lock is held.
func lockActivation(home, app, principal string) (*lockfile.Lock, error) {
	if home == "" {
		home = Home()
	}
	principal = strings.TrimSpace(principal)
	if principal == "" || principalSlug(principal) == "" {
		return nil, fmt.Errorf("src: activation needs a usable principal")
	}
	if _, err := repoDir(home, app); err != nil {
		return nil, err
	}
	st, err := state.OpenAt(home, app, principal)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(st.Dir(), 0700); err != nil {
		return nil, err
	}
	return lockfile.AcquireWithin(filepath.Join(st.Dir(), ".activation.lock"), 30*time.Second, lockfile.Holder{Name: principal, Intent: "activate, restore or adopt an app version"})
}
