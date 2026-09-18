package meet

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// safeSeenName is copied from pkg/bus. Reader names come from flags,
// principals, and fleet names, then become one path segment under seen/.
var safeSeenName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func seenPath(id, reader string) (string, error) {
	dir, err := storeDir(id)
	if err != nil {
		return "", err
	}
	name := safeSeenName.ReplaceAllString(strings.TrimSpace(reader), "_")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "anonymous"
	}
	return filepath.Join(dir, "seen", name), nil
}

// SeenSeq is the last parsed transcript event this reader has been shown.
//
// A missing cursor means zero, but a room is not a public board: new invitees
// should be seeded with SeedCursor so they start at the room head.
func SeenSeq(id, reader string) int64 {
	p, err := seenPath(id, reader)
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// MarkSeen advances this reader to the room's parsed transcript head.
func MarkSeen(id, reader string) error {
	events, err := readRoomTranscript(id)
	if err != nil {
		return err
	}
	return markSeenSeq(id, reader, int64(len(events)))
}

// MarkSeenThrough advances a reader only through the supplied transcript
// sequence. It is the acknowledgement half of UnreadThrough: callers render a
// stable snapshot first, then acknowledge precisely that snapshot. New events
// appended while output is being written remain unread.
func MarkSeenThrough(id, reader string, seq int64) error {
	return markSeenSeq(id, reader, seq)
}

// SeedCursor opens a new reader at HEAD. Unlike the public message board, a
// meeting room must not hand a freshly invited participant the whole backlog.
func SeedCursor(id, reader string) error {
	return MarkSeen(id, reader)
}

func markSeenSeq(id, reader string, seq int64) error {
	if seq <= SeenSeq(id, reader) {
		return nil
	}
	p, err := seenPath(id, reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("meet: creating seen cursor directory: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o644); err != nil {
		return fmt.Errorf("meet: writing seen cursor: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("meet: writing seen cursor: %w", err)
	}
	return nil
}
