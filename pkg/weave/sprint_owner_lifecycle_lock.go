package weave

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/spf13/cobra"
)

var sprintOwnerLifecycleWait = 2 * time.Minute

var sprintOwnerLifecycleMu sync.Map // sprint id -> *sync.Mutex

// withSprintOwnerLifecycle serializes slow managed-session transitions for one
// sprint without monopolizing queue.lock, which protects every sprint on the
// host. The per-sprint lock is process-safe; callers use queue.lock only for
// their short validation/commit phases.
func withSprintOwnerLifecycle(ctx context.Context, id int64, intent string, fn func() error) error {
	local, _ := sprintOwnerLifecycleMu.LoadOrStore(id, &sync.Mutex{})
	local.(*sync.Mutex).Lock()
	defer local.(*sync.Mutex).Unlock()
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	l, err := lockfile.AcquireWithin(filepath.Join(dir, "owner-lifecycle", fmt.Sprintf("%d.lock", id)), sprintOwnerLifecycleWait,
		lockfile.Holder{Name: fmt.Sprintf("sprint-%d-owner", id), Intent: intent})
	if err != nil {
		return fmt.Errorf("sprint #%d owner lifecycle: %w", id, err)
	}
	defer l.Release()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fn()
	}
}

func runSprintOwnerLifecycle(cmd *cobra.Command, flags *weaveOutputFlags, id int64, op, intent string, fn func() error) error {
	err := withSprintOwnerLifecycle(cmd.Context(), id, intent, fn)
	if err == nil {
		return nil
	}
	var coded *exitCodeError
	if errors.As(err, &coded) {
		return err
	}
	return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), op, weavecli.ExitGenericFail, err))
}

// sprintOwnerSnapshot reads the small identity/lifecycle state while holding
// queue.lock, then releases it before a host Start/Stop callback can block.
func sprintOwnerSnapshot(id int64) (*weaveStory, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	var snapshot *weaveStory
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		copy := *s
		snapshot = &copy
		return nil
	})
	return snapshot, err
}

func stopRecordedSprintOwner(ctx context.Context, s *weaveStory, cwd string) error {
	if s == nil {
		return nil
	}
	owner := strings.TrimSpace(s.Owner)
	if owner == "" || StopSprintOwner == nil {
		return nil
	}
	return retireSprintOwnerSession(ctx, s.ID, owner, cwd)
}
