package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"
)

const spoolLockWait = time.Second

// lockSpool never waits indefinitely on the host's telemetry path. Contention
// is returned to the caller as an error and reported through the spool's
// notice channel, not converted to silent success — the rotation path this
// guards is exactly the one whose failures used to be invisible while the
// disk filled.
func lockSpool(ctx context.Context, f *os.File) (func() error, error) {
	ctx, cancel := context.WithTimeout(ctx, spoolLockWait)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("telemetry: spool lock: %w", err)
		}
		ok, release, err := trySpoolLock(f)
		if err != nil {
			return nil, fmt.Errorf("telemetry: spool lock: %w", err)
		}
		if ok {
			return release, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("telemetry: spool lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
