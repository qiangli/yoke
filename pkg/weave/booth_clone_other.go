//go:build !darwin && !linux

package weave

import (
	"io"
	"os"
)

func boothCloneFile(src, dst string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	return "copy", closeErr
}
