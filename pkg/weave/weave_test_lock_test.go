package weave

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// weaveFlock keeps the older contention tests focused on their package-level
// behavior while routing every actual lock through the one shared primitive.
func weaveFlock(path string, wait time.Duration) (func(), error) {
	l, err := lockfile.AcquireWithin(path, wait, lockfile.Holder{
		Name: "weave-test", Intent: "hold test lock",
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return nil, weaveLockBusy{lock: filepath.Base(path), cause: err}
		}
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

func assertPullLockFree(t *testing.T, dir string) {
	t.Helper()
	release, err := weaveFlock(filepath.Join(dir, "pull.lock"), 0)
	if err != nil {
		t.Fatalf("pull.lock held for adversarial review: %v", err)
	}
	release()
}
