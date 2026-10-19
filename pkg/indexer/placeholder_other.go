//go:build !darwin && !windows

package indexer

import "io/fs"

func placeholder(fs.FileInfo) bool { return false }
