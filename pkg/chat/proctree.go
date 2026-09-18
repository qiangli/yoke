package chat

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// waitForProcessTree owns cancellation through BOTH process exit and pipe EOF.
// cmd must have been started with Cancel=nil and a nonzero WaitDelay: that
// remains the final bound if a descendant escapes the process group altogether.
// kill is supplied per command so a missed signal can be tested without a
// timing-dependent kernel race or a global test hook.
func waitForProcessTree(ctx context.Context, cmd *exec.Cmd, kill func() error) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	select {
	case err := <-done:
		return err
	default:
	}

	attempts := 0
	var first, last error
	signal := func() {
		last = kill()
		attempts++
		if attempts == 1 {
			first = last
		}
	}
	result := func(err error) error {
		// Keep the first branch/error as well as the final outcome. A five-second
		// fallback must not erase whether killpg succeeded or fell back to a pid.
		return errors.Join(ctx.Err(),
			fmt.Errorf("process-tree teardown: attempts=%d first=%v last=%v wait=%v", attempts, first, last, err),
			first, last, err)
	}
	signal()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Once the group is gone, do not signal that numeric id again: it could
		// eventually be reused by another job.
		if processTreeGone(last) {
			return result(<-done)
		}
		select {
		case err := <-done:
			return result(err)
		case <-ticker.C:
			select {
			case err := <-done:
				return result(err)
			default:
			}
			signal()
		}
	}
}
