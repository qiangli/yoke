//go:build !windows

package broker

import "os"

// secureDir makes dir owner-only; files written inside it are 0600.
func secureDir(dir string) error { return os.Chmod(dir, 0o700) }
