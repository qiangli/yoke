//go:build unix

package loom

import "os"

func defaultUID() int {
	if os.Geteuid() == 0 || os.Getuid() == 0 {
		return 0
	}
	return os.Geteuid()
}

func isUnix() bool {
	return true
}
