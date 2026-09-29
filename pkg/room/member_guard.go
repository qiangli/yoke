package room

import (
	"os"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// WithMemberClaimsGuard runs a short maintenance operation while new room
// identities cannot be claimed. Contention fails immediately; maintenance can
// retry on the next lifecycle transition. The callback may call Members.
func WithMemberClaimsGuard(fn func() error) error {
	if _, err := membersDir(); err != nil {
		return err
	}
	held, err := lockfile.TryAcquire(memberClaimsLockPath(), lockfile.Holder{
		Name: "room-maintenance", PID: os.Getpid(), Intent: "guard member lifecycle",
	})
	if err != nil {
		return err
	}
	defer held.Release()
	return fn()
}
