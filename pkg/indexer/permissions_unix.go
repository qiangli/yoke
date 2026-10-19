//go:build !windows

package indexer

import "os"

func secureDirectory(path string) error { return os.Chmod(path, 0700) }
