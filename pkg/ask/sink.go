package ask

import (
	"fmt"
	"os"
	"path/filepath"
)

// The sinks. Which one is chosen is the security decision of this command, so it
// is recorded on the request and shown to the human before they type.
const (
	// SinkFile is the default: a 0600 file inside the private ask directory. The
	// agent receives the PATH on stdout, never the value.
	SinkFile = "file"
	// SinkOut is the same, at an operator-named path.
	SinkOut = "out"
	// SinkStdout prints the value. Explicit opt-in only.
	SinkStdout = "stdout"
)

// deliver writes the answered value to its destination and returns what should be
// printed on stdout.
//
// The asymmetry between the return values IS the feature: for the file sinks the
// caller prints a path, and the value never crosses the boundary into whatever is
// reading our stdout. Only SinkStdout returns the value itself, and only because
// the operator asked for it in so many words.
func deliver(r Request, value []byte) (stdout string, err error) {
	switch r.Sink.Kind {
	case SinkStdout:
		return string(value), nil
	case SinkOut:
		if err := writeSecretFile(r.Sink.Detail, value); err != nil {
			return "", err
		}
		return r.Sink.Detail, nil
	default:
		path := filepath.Join(requestDir(r.ID), valueFile)
		if err := writeSecretFile(path, value); err != nil {
			return "", err
		}
		return path, nil
	}
}

// writeSecretFile creates a file that only we can read, refusing every path shape
// that would quietly hand the contents to somebody else.
//
// Each guard corresponds to a way the naive `echo "$v" > /tmp/x` fails:
//
//   - O_EXCL: refuse to write a path that already exists. Without it, another
//     user who pre-created the path owns a file we are about to fill with a
//     credential, and os.WriteFile would happily oblige.
//   - O_NOFOLLOW: refuse a symlink. This is the classic /tmp attack — the
//     attacker points the name at a file in their own directory (or at something
//     of ours they want destroyed) and we follow it.
//   - 0600 at creation, not chmod afterwards: a create-then-chmod leaves a window
//     in which the file exists and is world-readable.
//   - a parent directory check: 0600 on the file is worthless if the directory
//     containing it is writable by others, because they can replace the file.
func writeSecretFile(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := checkParentDir(dir); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("ask: refusing to overwrite %s — remove it or choose another path", path)
		}
		return fmt.Errorf("ask: writing %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(value); err != nil {
		return fmt.Errorf("ask: writing %s: %w", path, err)
	}
	return nil
}

// checkParentDir refuses to place a secret in a directory others can write.
//
// A group- or world-writable parent means another user can unlink our file and
// substitute their own, or simply wait for us to create it and then read it if the
// directory is also readable. The file's own mode cannot defend against that.
func checkParentDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("ask: %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("ask: %s is not a directory", dir)
	}
	return checkParentDirAccess(dir, fi)
}
