package bus

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

// Drain positions live beside the timeline they index, so an isolated room
// (BASHY_ROOM_DIR) gets isolated cursors and a test never inherits a real one.
//
// pkg/room's timeline retention (room/archive.go) reads this same directory
// directly rather than importing this package — pkg/bus already imports
// pkg/room, so the reverse import would cycle. That makes THIS FORMAT a
// cross-package contract: one bare int64 per file, named for the sanitized
// subscriber, mtime is the last successful (non-peek) drain. Changing either
// without the other silently breaks retention's staleness judgment.
const cursorDir = "cursors"

// resolveSubscriber names the drain cursor.
//
// Per-subscriber, not per-topic: two agents draining the same topic must each
// get their own copy, or the first to drain would consume the other's messages.
// That is the difference between a bus and a queue, and it is decided here.
func resolveSubscriber(as string) string {
	if s := strings.TrimSpace(as); s != "" {
		return s
	}
	for _, k := range []string{"BASHY_PRINCIPAL", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "anonymous"
}

// safeName keeps a subscriber name from escaping the cursor directory. The name
// comes from a flag or the environment and becomes a path segment, so `--as
// ../../etc/passwd` must not be able to write outside it.
var safeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func cursorPath(subscriber string) (string, error) {
	name := safeName.ReplaceAllString(subscriber, "_")
	name = strings.TrimLeft(name, ".") // no leading dots, so never "." or ".."
	if name == "" {
		name = "anonymous"
	}
	dir := filepath.Join(room.Dir(), cursorDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("bus: creating %s: %w", dir, err)
	}
	return filepath.Join(dir, name), nil
}

// readCursor returns the last sequence number this subscriber drained.
//
// A missing cursor is 0, meaning "you have seen nothing" — so a first drain
// delivers the whole backlog rather than silently skipping to the end. Starting
// at the end would mean a subscriber's first drain reports "nothing new" for
// messages that were published specifically for it.
func readCursor(subscriber string) (int64, error) {
	path, err := cursorPath(subscriber)
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
	if err != nil {
		// A corrupt cursor rewinds to the beginning rather than failing: the
		// worst case is re-delivering messages, which is recoverable, while
		// refusing to drain at all is not.
		return 0, nil
	}
	return n, nil
}

func writeCursor(subscriber string, seq int64) error {
	path, err := cursorPath(subscriber)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("bus: writing the drain cursor: %w", err)
	}
	// Rename, so a crash mid-write leaves the OLD position rather than a
	// truncated one. Re-reading a message is harmless; skipping one is not.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("bus: writing the drain cursor: %w", err)
	}
	return nil
}
