// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// ErrLockUnsupported is returned by every MUTATION on a platform with no file locking
// (js/wasm, plan9, …). Reads keep working: they never take the lock.
//
// An earlier revision shipped a no-op lock on those platforms, with an apology in the
// comment. That is worse than no lock at all, and the apology is what gives it away: a
// caller who takes the lock believes the read/decide/write cycle is serialized. It is
// not. Two agents interleave, both replay a vacant seat, both append a claim, and the
// host now has two stewards that each believe they are the only one — the exact failure
// the singleton exists to prevent, produced by the mechanism meant to enforce it.
//
// The fencing epoch does not save this, either. Both claims mint their epoch from the
// same replayed head, so they COLLIDE rather than supersede: neither steward is fenced,
// because neither one's token is stale.
//
// So the seat fails closed. A platform that cannot serialize cannot host a steward, and
// saying so is the only honest option.
//
// It is declared here, not in the platform file, so every build can name it — including
// the test that proves every mutation fails closed when it is returned
// (TestUnsupportedLockFailsEveryMutationClosed).
var ErrLockUnsupported = errors.New(
	"steward: this platform has no file locking, so the seat's read-decide-write cycle cannot be serialized — " +
		"refusing to mutate rather than risk two stewards on one host. Reads (status, board, log, reconcile) still work")

func lockFile(f *os.File) (func(), error) {
	if !LockSupported() {
		return nil, ErrLockUnsupported
	}
	l, err := lockfile.Acquire(f.Name(), lockfile.Holder{
		Name: "steward-seat", PID: os.Getpid(), Intent: "update steward state", Since: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

// LockSupported reports whether this platform can host a steward seat.
func LockSupported() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly", "solaris", "windows":
		return true
	default:
		return false
	}
}
