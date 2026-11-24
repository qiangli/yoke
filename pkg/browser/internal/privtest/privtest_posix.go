//go:build !windows

package privtest

import (
	"fmt"
	"os"
)

func check(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0o600 {
		return fmt.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	return nil
}
