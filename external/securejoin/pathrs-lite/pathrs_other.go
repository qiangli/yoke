//go:build !linux

package pathrs

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("pathrs: only supported on linux")

// OpenInRoot is unsupported off Linux.
func OpenInRoot(string, string) (*os.File, error) { return nil, errUnsupported }

// OpenatInRoot is unsupported off Linux.
func OpenatInRoot(*os.File, string) (*os.File, error) { return nil, errUnsupported }

// MkdirAllHandle is unsupported off Linux.
func MkdirAllHandle(*os.File, string, os.FileMode) (*os.File, error) { return nil, errUnsupported }

// Reopen is unsupported off Linux.
func Reopen(*os.File, int) (*os.File, error) { return nil, errUnsupported }
