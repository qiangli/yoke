package foreman

import "fmt"

// Terminate is unavailable where managed control sessions are (see
// ControlSupported).
func Terminate(root, id, reason string) error {
	return fmt.Errorf("%w: managed control sessions are not supported on native Windows", ErrServeNotProven)
}
