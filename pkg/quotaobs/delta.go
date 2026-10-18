package quotaobs

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
)

// Cursor records positions in the active journal and its rotated predecessor.
// A missing or replaced unread file expires the cursor rather than losing rows.
type Cursor struct {
	ActiveID      uint64 `json:"active_id"`
	ActiveOffset  int64  `json:"active_offset"`
	ActiveAnchor  string `json:"active_anchor"`
	RotatedID     uint64 `json:"rotated_id"`
	RotatedOffset int64  `json:"rotated_offset"`
	RotatedAnchor string `json:"rotated_anchor"`
}

var ErrCursorExpired = errors.New("quota observation cursor expired")

func identity(f *os.File) (uint64, error) {
	s, err := f.Stat()
	if err != nil {
		return 0, err
	}
	v := reflect.ValueOf(s.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() {
		return 0, ErrCursorExpired
	}
	i := v.FieldByName("Ino")
	if !i.IsValid() || !i.CanUint() {
		return 0, ErrCursorExpired
	}
	return i.Uint(), nil
}

func anchor(f *os.File, offset int64) (string, error) {
	start := max(int64(0), offset-256)
	b := make([]byte, offset-start)
	if _, err := f.ReadAt(b, start); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func deltaFile(name string, id uint64, offset int64, priorAnchor string) ([]Observation, uint64, int64, string, error) {
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		if id != 0 {
			return nil, 0, 0, "", ErrCursorExpired
		}
		return nil, 0, 0, "", nil
	}
	if err != nil {
		return nil, 0, 0, "", err
	}
	defer f.Close()
	current, err := identity(f)
	if err != nil {
		return nil, 0, 0, "", err
	}
	s, err := f.Stat()
	if err != nil {
		return nil, 0, 0, "", err
	}
	if id != 0 && (id != current || offset > s.Size()) {
		return nil, 0, 0, "", ErrCursorExpired
	}
	if id != 0 {
		got, err := anchor(f, offset)
		if err != nil {
			return nil, 0, 0, "", err
		}
		if got != priorAnchor {
			return nil, 0, 0, "", ErrCursorExpired
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, 0, "", err
	}
	r := bufio.NewReader(f)
	var out []Observation
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		} // Retry incomplete append next tick.
		if err != nil {
			return nil, 0, 0, "", err
		}
		var o Observation
		if err := json.Unmarshal(line, &o); err != nil {
			return nil, 0, 0, "", fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, o)
		offset += int64(len(line))
	}
	endAnchor, err := anchor(f, offset)
	return out, current, offset, endAnchor, err
}

// ReadDelta reads complete records after a durable cursor. Rotation moves the
// old active file to .1; a cursor that misses two rotations expires explicitly.
func ReadDelta(c Cursor) ([]Observation, Cursor, error) {
	p, err := path()
	if err != nil {
		return nil, c, err
	}
	rotated, rid, roff, ra, err := deltaFile(p+".1", c.RotatedID, c.RotatedOffset, c.RotatedAnchor)
	if errors.Is(err, ErrCursorExpired) {
		// A second rotation replaced the predecessor. Resume from the old
		// active inode if it is now the predecessor; otherwise expire.
		rotated, rid, roff, ra, err = deltaFile(p+".1", c.ActiveID, c.ActiveOffset, c.ActiveAnchor)
	}
	if err != nil {
		return nil, c, err
	}
	active, aid, aoff, aa, err := deltaFile(p, c.ActiveID, c.ActiveOffset, c.ActiveAnchor)
	if errors.Is(err, ErrCursorExpired) && c.ActiveID != 0 && rid == c.ActiveID {
		// Rotation happened between ticks: resume the old active at its
		// previous offset in .1, then start the new active at zero.
		rotated, rid, roff, ra, err = deltaFile(p+".1", c.ActiveID, c.ActiveOffset, c.ActiveAnchor)
		if err == nil {
			active, aid, aoff, aa, err = deltaFile(p, 0, 0, "")
		}
	}
	if err != nil {
		return nil, c, err
	}
	return append(rotated, active...), Cursor{ActiveID: aid, ActiveOffset: aoff, ActiveAnchor: aa, RotatedID: rid, RotatedOffset: roff, RotatedAnchor: ra}, nil
}
