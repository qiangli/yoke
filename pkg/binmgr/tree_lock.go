package binmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// Serialize tree cache inspection through publication. A second installer
// must not remove a tree the first installer has already returned to a caller
// (Windows may allow partial deletion before refusing the running executable).
func lockTree(ctx context.Context, dir string) (*lockfile.Lock, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, err := lockfile.TryAcquire(filepath.Join(dir, ".tree.lock"), lockfile.Holder{Intent: "provision binary tree"})
		if !errors.Is(err, lockfile.ErrHeld) {
			return l, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
