package room

// Timeline archive rotation — the same three-condition rule as the message
// board (pkg/bus/archive.go), applied to timeline.jsonl instead of
// posts.jsonl and keyed on the bus's per-subscriber DRAIN CURSORS
// (pkg/bus/cursor.go's cursors/<subscriber> files) instead of the board's
// seen/ cursors.
//
// This package cannot import pkg/bus — pkg/bus already imports pkg/room, and
// the reverse would cycle — so cursorStates below reads that directory
// directly. See cursor.go's doc comment in pkg/bus for the format contract
// this depends on.
//
// # The rule
//
// An event archives only when ALL THREE hold:
//
//  1. it is older than RetentionWindow (default 7 days);
//  2. every subscriber who has drained within the window (non-stale) has a
//     cursor past it. Both inbox and watch advance a subscriber's cursor to
//     the current timeline HEAD on every non-peek drain, matched or not —
//     see their `high` tracking — so unlike the board, no per-event relevance
//     filter is needed here: a subscriber's cursor value alone says how
//     recently they have seen the whole timeline, not just their slice of it;
//  3. it is not an unread event DIRECTED at one recipient (Event.To) — that
//     recipient's cursor must have passed it NO MATTER HOW STALE, so an
//     obligation never disappears just because its recipient has been away
//     longer than the window.
//
// # No renumbering
//
// Event.Seq has never been stored on disk (Emit always writes it as the zero
// value) — Timeline recomputes it from LINE POSITION on every read, which is
// what let a naive rotation renumber every surviving event by deleting a
// prefix. archivedThrough is the fix: Timeline adds it as a base offset, so a
// live event's seq stays exactly what it was before any rotation ever
// touched the file, even though the file itself only holds the tail.
//
// # No daemon
//
// Rotation runs opportunistically from Emit, throttled by the same
// stat-only check the board uses, for the same reason: correctness never
// depends on it running on any particular write, only eventually.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// DefaultRetentionWindow mirrors pkg/bus's board default: an event is
// eligible for archiving after this long, never a guarantee.
const DefaultRetentionWindow = 7 * 24 * time.Hour

// RetentionWindow reads $BASHY_ROOM_RETENTION (a Go duration string, e.g.
// "168h"), falling back to DefaultRetentionWindow.
func RetentionWindow() time.Duration {
	if v := strings.TrimSpace(os.Getenv("BASHY_ROOM_RETENTION")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultRetentionWindow
}

const rotationCheckInterval = 15 * time.Minute

func archiveDir() string { return filepath.Join(Dir(), "archive") }

func archiveWatermarkPath() string { return filepath.Join(archiveDir(), ".watermark") }

func rotationMarkerPath() string { return filepath.Join(archiveDir(), ".last-attempt") }

func timelineLockPath() string { return filepath.Join(Dir(), ".timeline.lock") }

// archivedThrough is the highest event sequence already moved to the
// archive. Zero when nothing has ever been archived.
func archivedThrough() int64 {
	b, err := os.ReadFile(archiveWatermarkPath())
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func writeArchiveWatermark(seq int64) error {
	if err := os.MkdirAll(archiveDir(), 0o700); err != nil {
		return err
	}
	tmp := archiveWatermarkPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, archiveWatermarkPath())
}

func archiveMonthKey(e Event) string {
	if t, err := time.Parse(time.RFC3339, e.TS); err == nil {
		return t.UTC().Format("2006-01")
	}
	return "unknown"
}

// cursorSafeName mirrors pkg/bus/cursor.go's safeName exactly — it has to,
// since this reads files that package names. Duplicated rather than
// imported to avoid the import cycle; see the package doc.
var cursorSafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func cursorFileName(subscriber string) string {
	name := cursorSafeName.ReplaceAllString(strings.TrimSpace(subscriber), "_")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "anonymous"
	}
	return name
}

// cursorState is one subscriber's drain position and last successful
// (non-peek) drain time, read straight from pkg/bus's cursor store.
type cursorState struct {
	fileName string
	seq      int64
	lastPoll time.Time
}

func cursorStates() []cursorState {
	dir := filepath.Join(Dir(), "cursors")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]cursorState, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if perr != nil {
			continue
		}
		var t time.Time
		if info, ierr := e.Info(); ierr == nil {
			t = info.ModTime()
		}
		out = append(out, cursorState{fileName: e.Name(), seq: n, lastPoll: t})
	}
	return out
}

// canArchiveEvent applies retention conditions 2 and 3. Condition 1 (age) is
// checked by the caller.
func canArchiveEvent(e Event, cursors []cursorState, now time.Time, window time.Duration) bool {
	for _, c := range cursors {
		if now.Sub(c.lastPoll) > window {
			continue // stale — excluded, per condition 2
		}
		if c.seq < e.Seq {
			return false
		}
	}

	to := strings.TrimSpace(e.To)
	if to == "" {
		return true
	}
	// Condition 3: an event naming one recipient is protected until THAT
	// recipient's own cursor has passed it, regardless of staleness. No
	// cursor under that name at all means nobody has ever proven they
	// drained past it — safer to treat as unread than to guess.
	want := cursorFileName(to)
	for _, c := range cursors {
		if c.fileName == want {
			return c.seq >= e.Seq
		}
	}
	return false
}

// RotateTimeline archives the oldest run of events that satisfy all three
// retention conditions, appending them to a monthly archive file and
// shrinking timeline.jsonl to what remains.
func RotateTimeline(now time.Time) (archived int, err error) {
	dir := Dir()
	if dir == "" {
		return 0, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}

	lock, lerr := lockfile.TryAcquire(timelineLockPath(), lockfile.Holder{
		Name: "room-rotate", PID: os.Getpid(), Intent: "archive",
	})
	if lerr != nil {
		return 0, nil
	}
	defer lock.Release()

	all, err := Timeline(0)
	if err != nil || len(all) == 0 {
		return 0, err
	}
	window := RetentionWindow()
	cutoff := now.Add(-window)
	cursors := cursorStates()

	var toArchive []Event
	for _, e := range all {
		ts, terr := time.Parse(time.RFC3339, e.TS)
		if terr != nil || !ts.Before(cutoff) {
			break
		}
		if !canArchiveEvent(e, cursors, now, window) {
			break
		}
		toArchive = append(toArchive, e)
	}
	if len(toArchive) == 0 {
		return 0, nil
	}

	if err := appendTimelineArchive(toArchive); err != nil {
		return 0, err
	}
	remaining := all[len(toArchive):]
	if err := rewriteTimeline(remaining); err != nil {
		return 0, err
	}
	if err := writeArchiveWatermark(toArchive[len(toArchive)-1].Seq); err != nil {
		return 0, err
	}
	return len(toArchive), nil
}

// appendTimelineArchive writes events into their monthly files WITH their
// resolved seq baked in — unlike the live file, the archive is the permanent
// record prose refers to, so its copy must carry the real identity rather
// than the always-zero placeholder Emit writes.
func appendTimelineArchive(events []Event) error {
	if err := os.MkdirAll(archiveDir(), 0o700); err != nil {
		return err
	}
	byMonth := map[string][]Event{}
	var order []string
	for _, e := range events {
		key := archiveMonthKey(e)
		if _, ok := byMonth[key]; !ok {
			order = append(order, key)
		}
		byMonth[key] = append(byMonth[key], e)
	}
	for _, key := range order {
		path := filepath.Join(archiveDir(), key+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		for _, e := range byMonth[key] {
			line, merr := json.Marshal(e)
			if merr != nil {
				f.Close()
				return merr
			}
			if _, werr := f.Write(append(line, '\n')); werr != nil {
				f.Close()
				return werr
			}
		}
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
	}
	return nil
}

// rewriteTimeline replaces timeline.jsonl with exactly these events, in
// order. Seq is reset to zero before writing — matching Emit's convention
// that the live file never stores it, since Timeline recomputes it from
// position plus the archive offset on every read. Called only while holding
// timelineLockPath, so this can never race a concurrent Emit.
func rewriteTimeline(events []Event) error {
	var b strings.Builder
	for _, e := range events {
		e.Seq = 0
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path, err := timelinePath()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shouldAttemptRotation(now time.Time) bool {
	info, err := os.Stat(rotationMarkerPath())
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) >= rotationCheckInterval
}

func touchRotationMarker() {
	p := rotationMarkerPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	now := time.Now()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		_ = os.WriteFile(p, nil, 0o600)
		return
	}
	_ = os.Chtimes(p, now, now)
}

// rotateTimelineOpportunistic is what Emit calls after every append:
// throttled, best-effort, never allowed to fail the write that triggered it.
func rotateTimelineOpportunistic() {
	now := time.Now()
	if !shouldAttemptRotation(now) {
		return
	}
	touchRotationMarker()
	_, _ = RotateTimeline(now)
}
