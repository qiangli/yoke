//go:build linux

package weave

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
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
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
		return "reflink", out.Close()
	}
	if err := out.Truncate(0); err != nil {
		_ = out.Close()
		return "", err
	}
	if _, err := in.Seek(0, 0); err != nil {
		_ = out.Close()
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	return "copy", closeErr
}
