package feed

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The feed's own spool cursor: one int64 per subscriber under
// KeyDir/cursors/<safe name>. It mirrors pkg/bus/cursor.go's format and
// atomic temp+rename write but indexes the feed spool, not the bus timeline.

func cursorFile(dir, subscriber string) (string, error) {
	cdir := filepath.Join(dir, "cursors")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		return "", fmt.Errorf("feed: creating %s: %w", cdir, err)
	}
	return filepath.Join(cdir, safeFileName(subscriber)), nil
}

// ReadCursor returns the last spool seq the subscriber has read; a missing
// or corrupt cursor is 0 (everything unread).
func ReadCursor(dir, subscriber string) (int64, error) {
	path, err := cursorFile(dir, subscriber)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, nil
	}
	return n, nil
}

// WriteCursor stores seq atomically, so a crash leaves the old position
// rather than a truncated one.
func WriteCursor(dir, subscriber string, seq int64) error {
	path, err := cursorFile(dir, subscriber)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("feed: writing the read cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("feed: writing the read cursor: %w", err)
	}
	return nil
}
