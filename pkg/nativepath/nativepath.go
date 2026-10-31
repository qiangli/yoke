// Package nativepath makes the process PATH readable by Go's os/exec on Windows.
//
// Inside the bashy shell on Windows PATH is exported in the POSIX spelling
// ("/c/bashy:/c/Windows"), which exec.LookPath splits on ';' and so never
// finds anything. Importing this package normalizes the process PATH to the
// native spelling once, so every in-process child exec sees a usable PATH
// without per-call-site resolution. It also collapses a block that carries
// PATH under several spellings ("Path" from the OS, "PATH" from a shell
// export) to the one the process reads (see pathenv.go). It is a no-op off
// Windows.
package nativepath

import (
	"os"
	"runtime"

	"mvdan.cc/sh/v3/pathconv"
)

func init() {
	if runtime.GOOS != "windows" {
		return
	}
	collapseProcessPath()
	if p := os.Getenv("PATH"); p != "" {
		if n := Normalize(p); n != p {
			_ = os.Setenv("PATH", n)
		}
	}
}

// Normalize returns pathValue as a native ';'-separated Windows path list.
// It is GOOS-independent: it converts MSYS/POSIX elements regardless of host.
func Normalize(pathValue string) string {
	return pathconv.NativePathList(pathValue)
}
