//go:build darwin

package weave

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func boothCloneFile(src, dst string, mode os.FileMode) (string, error) {
	if err := unix.Clonefile(src, dst, 0); err == nil {
		return "clonefile", nil
	}
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
