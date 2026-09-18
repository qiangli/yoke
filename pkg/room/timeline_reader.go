package room

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// TimelinePosition identifies the latest matching event within a stream
// generation. Consumers must rebase pending work when Generation changes:
// historical records in a replacement stream are not new acknowledgments.
// Legitimate archive rotation preserves the generation and observed match.
type TimelinePosition struct {
	Generation uint64
	Seq        int64
}

// TimelineReadStats counts timeline IO and JSON work (metadata excluded).
// BytesRead includes bounded continuity checks on appends. Unchanged polls
// perform neither timeline reads nor decoding.
type TimelineReadStats struct{ BytesRead, BytesDecoded, RecordsDecoded, Resets int64 }

// TimelineReader reduces a timeline to the latest matching event. One caller
// owns a reader; it is not safe for concurrent use. No file descriptor or event
// history is retained between calls. Only one unfinished trailing record is
// buffered until its newline arrives; memory is bounded by the largest record,
// rather than timeline history. The supported writer protocol is Emit's
// append and RotateTimeline's locked replacement + watermark publication.
// Bounded prefix/end checkpoints also detect ordinary in-place truncation and
// regrowth. Arbitrary edits that preserve both checkpoints while growing the
// file are outside that protocol; detecting those requires a full-history hash.
type TimelineReader struct {
	match                 func(Event) bool
	stats                 TimelineReadStats
	root                  string
	info, watermark       os.FileInfo
	offset, logical, base int64
	readOffset            int64
	partial               []byte
	position              TimelinePosition
	head, tail            []byte
}

func NewTimelineReader(match func(Event) bool) *TimelineReader { return &TimelineReader{match: match} }
func (r *TimelineReader) Stats() TimelineReadStats             { return r.stats }
func sameTimelineInfo(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
func statTimeline(path string) (os.FileInfo, error) {
	f, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return f, err
}
func (r *TimelineReader) reset() {
	r.position.Generation++
	r.position.Seq = 0
	r.offset = 0
	r.readOffset = 0
	r.partial = nil
	r.logical = 0
	r.base = 0
	r.info = nil
	r.watermark = nil
	r.head = nil
	r.tail = nil
	r.stats.Resets++
}

// Latest returns a coherent file/watermark snapshot under the rotation lock
// whenever metadata changed. The unchanged fast path may return the previous
// coherent snapshot during an in-flight rotation; it never invents a new ack.
// Lock contention is cancellable and bounded to two seconds. Reading is capped
// at the captured size because Emit may append despite lock timeout.
func (r *TimelineReader) Latest(ctx context.Context) (TimelinePosition, error) {
	next := *r
	position, err := next.latest(ctx)
	if err == nil {
		*r = next
	} else {
		r.stats = next.stats
	}
	return position, err
}

func (r *TimelineReader) latest(ctx context.Context) (TimelinePosition, error) {
	if err := ctx.Err(); err != nil {
		return r.position, err
	}
	root := filepath.Clean(Dir())
	path := filepath.Join(root, "timeline.jsonl")
	watermark := filepath.Join(root, "archive", ".watermark")
	if root != r.root {
		r.reset()
		r.root = root
	}
	info, err := statTimeline(path)
	if err != nil {
		return r.position, err
	}
	wm, err := statTimeline(watermark)
	if err != nil {
		return r.position, err
	}
	if sameTimelineInfo(info, r.info) && sameTimelineInfo(wm, r.watermark) {
		return r.position, nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var lock *lockfile.Lock
	for {
		if err := lockCtx.Err(); err != nil {
			return r.position, err
		}
		lock, err = lockfile.TryAcquire(filepath.Join(root, ".timeline.lock"), lockfile.Holder{Name: "room-reader", Intent: "snapshot"})
		if err == nil {
			break
		}
		if !errors.Is(err, lockfile.ErrHeld) {
			return r.position, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-lockCtx.Done():
			timer.Stop()
			return r.position, lockCtx.Err()
		case <-timer.C:
		}
	}
	defer lock.Release()
	wm, err = statTimeline(watermark)
	if err != nil {
		return r.position, err
	}
	base := int64(0)
	if wm != nil {
		// archivedThrough has the established malformed/missing-watermark fallback.
		base = archivedThrough()
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		r.reset()
		r.root = root
		r.watermark = wm
		return r.position, nil
	}
	if err != nil {
		return r.position, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return r.position, err
	}
	changed := r.info != nil
	rotation := changed && base > r.base
	appendOnly := changed && base == r.base && os.SameFile(info, r.info) && info.Size() > r.info.Size()
	if appendOnly {
		appendOnly, err = r.continuous(f)
		if err != nil {
			return r.position, err
		}
	}
	if changed && !appendOnly {
		if rotation {
			r.offset = 0
			r.readOffset = 0
			r.partial = nil
			r.logical = base
			r.head = nil
			r.tail = nil
		} else {
			r.reset()
			r.root = root
		}
	}
	if r.offset == 0 {
		r.logical = base
	}
	// Work on a copy so a cancelled/failed read cannot publish a partial snapshot.
	next := *r
	counter := &timelineCountingReader{reader: io.NewSectionReader(f, next.readOffset, info.Size()-next.readOffset), stats: &r.stats}
	reader := bufio.NewReaderSize(counter, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return r.position, err
		}
		line, readErr := reader.ReadBytes('\n')
		next.readOffset += int64(len(line))
		if readErr != nil {
			if readErr != io.EOF {
				return r.position, readErr
			}
			next.partial = append(next.partial, line...)
			break
		}
		if len(next.partial) > 0 {
			line = append(next.partial, line...)
			next.partial = nil
		}
		next.offset += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		r.stats.BytesDecoded += int64(len(line))
		r.stats.RecordsDecoded++
		var event Event
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		next.logical++
		event.Seq = next.logical
		if (r.match == nil || r.match(event)) && event.Seq > next.position.Seq {
			next.position.Seq = event.Seq
		}
	}
	next.head, next.tail, err = r.checkpoints(f, next.readOffset)
	if err != nil {
		return r.position, err
	}
	next.info = info
	next.watermark = wm
	next.base = base
	next.stats = r.stats
	*r = next
	return r.position, nil
}

type timelineCountingReader struct {
	reader io.Reader
	stats  *TimelineReadStats
}

func (c *timelineCountingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.stats.BytesRead += int64(n)
	return n, err
}

const timelineCheckpointBytes = 256

func (r *TimelineReader) checkpoints(f *os.File, offset int64) ([]byte, []byte, error) {
	n := min(offset, int64(timelineCheckpointBytes))
	if n == 0 {
		return nil, nil, nil
	}
	head := make([]byte, n)
	tail := make([]byte, n)
	a, err := f.ReadAt(head, 0)
	r.stats.BytesRead += int64(a)
	if err != nil {
		return nil, nil, err
	}
	b, err := f.ReadAt(tail, offset-n)
	r.stats.BytesRead += int64(b)
	return head, tail, err
}
func (r *TimelineReader) continuous(f *os.File) (bool, error) {
	head, tail, err := r.checkpoints(f, r.readOffset)
	return bytes.Equal(head, r.head) && bytes.Equal(tail, r.tail), err
}
