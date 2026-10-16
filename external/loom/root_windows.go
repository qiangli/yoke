//go:build windows

package loom

func defaultUID() int {
	return -1
}

func isUnix() bool {
	return false
}
