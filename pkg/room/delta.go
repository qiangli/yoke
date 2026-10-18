package room

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// ErrDeltaExpired means a reader missed retained timeline events or its file
// position was replaced without a room rotation.
var ErrDeltaExpired = errors.New("room delta cursor expired")

type deltaPosition struct {
	Root   string `json:"root"`
	Seq    int64  `json:"seq"`
	Offset int64  `json:"offset"`
	Inode  uint64 `json:"inode"`
	Anchor string `json:"anchor"`
}

func deltaInode(info os.FileInfo) uint64 {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() {
		return 0
	}
	i := v.FieldByName("Ino")
	if !i.IsValid() || !i.CanUint() {
		return 0
	}
	return i.Uint()
}

func deltaAnchor(f *os.File, offset int64) (string, error) {
	start := max(int64(0), offset-256)
	b := make([]byte, offset-start)
	if _, err := f.ReadAt(b, start); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

// ReadDelta reads complete room events after an opaque durable token. Appends
// start at the saved byte offset; archive rotation rebases by sequence. The
// room lock makes the file and archive watermark one coherent snapshot.
func ReadDelta(ctx context.Context, token string) ([]Event, string, error) {
	var p deltaPosition
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return nil, "", ErrDeltaExpired
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, "", ErrDeltaExpired
		}
		if p.Root != Dir() {
			return nil, "", ErrDeltaExpired
		}
	}
	if _, err := timelinePath(); err != nil {
		return nil, "", err
	}
	lock, err := lockfile.AcquireWithin(timelineLockPath(), 2*time.Second, lockfile.Holder{Name: "room-delta", PID: os.Getpid(), Intent: "read"})
	if err != nil {
		return nil, "", err
	}
	defer lock.Release()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	base := archivedThrough()
	if p.Seq != 0 && p.Seq < base {
		return nil, "", ErrDeltaExpired
	}
	path, err := timelinePath()
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if p.Seq != 0 && p.Seq > base {
			return nil, "", ErrDeltaExpired
		}
		b, _ := json.Marshal(deltaPosition{Root: Dir(), Seq: base})
		return nil, base64.RawURLEncoding.EncodeToString(b), nil
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	id := deltaInode(info)
	if id == 0 {
		return nil, "", ErrDeltaExpired
	}
	if p.Inode != 0 && p.Inode != id {
		if base == 0 {
			return nil, "", ErrDeltaExpired
		}
		p.Offset = 0
	}
	if p.Offset > info.Size() {
		return nil, "", ErrDeltaExpired
	}
	if p.Offset > 0 {
		a, err := deltaAnchor(f, p.Offset)
		if err != nil || a != p.Anchor {
			return nil, "", ErrDeltaExpired
		}
	}
	if _, err := f.Seek(p.Offset, io.SeekStart); err != nil {
		return nil, "", err
	}
	r := bufio.NewReader(f)
	seq := base
	if p.Offset > 0 {
		seq = p.Seq
	}
	var events []Event
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, "", fmt.Errorf("room delta: %w", err)
		}
		seq++
		p.Offset += int64(len(line))
		e.Seq = seq
		if e.Seq > p.Seq {
			events = append(events, e)
		}
	}
	a, err := deltaAnchor(f, p.Offset)
	if err != nil {
		return nil, "", err
	}
	p = deltaPosition{Root: Dir(), Seq: seq, Offset: p.Offset, Inode: id, Anchor: a}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	return events, base64.RawURLEncoding.EncodeToString(b), nil
}
