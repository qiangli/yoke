package bus

// Board archive rotation.
//
// The board is a durable log and reading it is the fast path every agent
// pays on every turn — Unseen already bounds that cost with a cursor. What
// was unbounded was the STORE ITSELF: nothing ever left posts.jsonl, so it
// grew forever and a bare `mb --history` (or anything that reads the whole file)
// grew with it. This is a hygiene fix, not a token fix — a short window still
// costs real tokens against the full history, because most of a live board
// was written recently. The point is BOUNDEDNESS: the live file stays sized
// to recent, still-relevant traffic, and nothing is ever lost — it moves to
// a monthly archive file instead.
//
// # The rule
//
// A post archives only when ALL THREE hold:
//
//  1. it is older than the retention window (RetentionWindow, default 7 days);
//  2. every READER who has polled within the window (a "non-stale" reader —
//     see boardReaderStates) has a cursor past it, for every post the reader
//     is even relevant to (Post.ForReader). A reader who has not polled in
//     over a week cannot block cleanup on that basis alone: a weave worker
//     can be away for days, and its absence must not be what keeps the board
//     growing forever;
//  3. it is not an unread DIRECTED post — see condition 3's own comment on
//     canArchivePost. This is what stops condition 2's staleness exclusion
//     from turning an outstanding obligation into a silent failure: the one
//     reader a directed post actually names must have read it, no matter how
//     long they have been away.
//
// Age alone is never sufficient — that is precisely the invariant this
// design protects: A READER MUST NEVER MISS A POST BECAUSE IT AGED OUT
// BETWEEN POLLS.
//
// # No renumbering
//
// A post's Seq is an identity, quoted in prose and in other stores (claims,
// views, receipts). Archiving must never renumber it. RotateBoard always
// archives a PREFIX — the oldest run of posts that qualify, stopping at the
// first that does not — so what remains is "everything after seq N" for a
// single N, recorded once as the archive watermark. PostMessageSeq then
// assigns new sequences starting after that watermark, so a seq is never
// reused even though posts.jsonl itself only holds the live tail.
//
// # No daemon
//
// Rotation runs opportunistically ON WRITE, inside PostMessageSeq — the way
// log rotation always has. There is nothing to start, nothing to crash,
// nothing that can silently stop running. A throttle (rotationCheckInterval)
// keeps the full-board scan this requires from running on literally every
// single post; it is a performance bound, not a correctness one — the worst
// case of skipping an attempt is that a post stays live a little longer than
// it strictly needed to.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// DefaultRetentionWindow is how long a post lives on the board before it is
// even ELIGIBLE for archiving. Eligible, not guaranteed: condition 2 and 3
// can hold a much older post live indefinitely.
const DefaultRetentionWindow = 7 * 24 * time.Hour

// RetentionWindow reads $BASHY_MB_RETENTION (a Go duration string, e.g.
// "168h"), falling back to DefaultRetentionWindow.
func RetentionWindow() time.Duration {
	if v := strings.TrimSpace(os.Getenv("BASHY_MB_RETENTION")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultRetentionWindow
}

// rotationCheckInterval throttles how often a write attempts the full-board
// scan RotateBoard needs. It bounds cost, not correctness: retention windows
// are measured in days, so checking every 15 minutes loses nothing that
// matters and keeps a busy board from re-scanning itself on every post.
const rotationCheckInterval = 15 * time.Minute

func archiveDir() string { return filepath.Join(BoardDir(), "archive") }

func archiveWatermarkPath() string { return filepath.Join(archiveDir(), ".watermark") }

func rotationMarkerPath() string { return filepath.Join(archiveDir(), ".last-attempt") }

func boardLockPath() string { return filepath.Join(BoardDir(), ".lock") }

// archivedThrough is the highest post sequence already moved to the archive.
// Zero when nothing has ever been archived — the same value PostMessageSeq
// always effectively used before this feature existed.
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
	if err := os.MkdirAll(archiveDir(), 0o755); err != nil {
		return err
	}
	tmp := archiveWatermarkPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, archiveWatermarkPath())
}

// archiveMonthKey names the monthly file a post rotates into: YYYY-MM, from
// the post's own timestamp so a post always files under when it was WRITTEN,
// not when it happened to be swept.
func archiveMonthKey(p Post) string {
	if t, err := time.Parse(time.RFC3339, p.At); err == nil {
		return t.UTC().Format("2006-01")
	}
	return "unknown"
}

// withBoardLock runs fn holding a short, best-effort cross-process lock.
//
// Best-effort deliberately: a lock that could fail a post (a sender's
// message not landing because a hygiene mechanism could not get a lock) would
// be a worse defect than the rare race it prevents. On any acquisition
// failure — contention past the bound, or advisory locking simply not being
// supported on this platform (see pkg/lockfile's unsupported build) — fn
// still runs, unlocked, which is exactly the pre-existing behavior this
// feature must not regress.
func withBoardLock(intent string, fn func()) {
	lock, err := lockfile.AcquireWithin(boardLockPath(), 2*time.Second, lockfile.Holder{
		Name: "mb", PID: os.Getpid(), Intent: intent,
	})
	if err == nil {
		defer lock.Release()
	}
	fn()
}

// boardReaderState is one reader's cursor and last-poll time, for judging
// retention. lastPoll is the seen/<reader> file's mtime: MarkSeen only
// touches it when there is something new for that reader to advance past, so
// this is an approximation ("last time something new was shown"), not a
// literal poll log — a reader that polls constantly but is never relevant to
// new traffic can look more stale than it is. That is an acceptable skew for
// a non-obligation post (condition 2 only ever makes archiving MORE
// conservative when it is wrong in this direction), and it never affects
// condition 3, which does not consult staleness at all.
type boardReaderState struct {
	name     string
	cursor   int64
	lastPoll time.Time
}

func boardReaderStates() []boardReaderState {
	names := boardReaders()
	out := make([]boardReaderState, 0, len(names))
	for _, n := range names {
		cur, _ := CursorSeq(n)
		var t time.Time
		if info, err := os.Stat(seenPath(n)); err == nil {
			t = info.ModTime()
		}
		out = append(out, boardReaderState{name: n, cursor: cur, lastPoll: t})
	}
	return out
}

// canArchivePost applies retention conditions 2 and 3 to one post. Condition
// 1 (age) is checked by the caller, which walks posts oldest-first and stops
// at the first one that is not old enough.
func canArchivePost(p Post, readers []boardReaderState, now time.Time, window time.Duration, audiences audienceSnapshot) bool {
	// Condition 2: every reader who has polled within the window, and for
	// whom this post is even relevant, has a cursor past it. A stale reader
	// (away longer than the window) does not get a vote here — see the
	// package doc.
	for _, r := range readers {
		if now.Sub(r.lastPoll) > window {
			continue
		}
		if p.forReader(r.name, audiences) && r.cursor < p.Seq {
			return false
		}
	}

	to := strings.TrimSpace(p.To)
	if to == "" {
		return true
	}
	if !AddressedToRole(to) {
		// Condition 3, the ordinary case: a post to one named agent is an
		// obligation on exactly that agent. Its cursor must have passed the
		// post NO MATTER HOW STALE IT IS — that is the whole reason this
		// condition exists, separate from condition 2's staleness carve-out.
		// No cursor at all means nobody has ever proven they read it: not
		// unread, UNVERIFIED, which is even less safe to sweep.
		cur, ok := CursorSeq(to)
		return ok && cur >= p.Seq
	}
	// A role address is directed at WHOEVER reads it (Post.Directed), so
	// there is no single cursor its mail can be proven read against. The
	// closest honest approximation: every reader this host currently knows
	// about, stale or not, must have passed it before it leaves the live
	// board — the seat may change hands, but somebody must have seen the
	// message first.
	for _, r := range readers {
		if p.Directed(r.name) && r.cursor < p.Seq {
			return false
		}
	}
	return true
}

// RotateBoard archives the oldest run of posts that satisfy all three
// retention conditions. Archiving always stops at the first post that does
// not qualify — see the package doc on why a prefix is what keeps the
// watermark a single integer.
func RotateBoard(now time.Time) (archived int, err error) {
	dir := BoardDir()
	if dir == "" {
		return 0, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}

	lock, lerr := lockfile.TryAcquire(boardLockPath(), lockfile.Holder{
		Name: "mb-rotate", PID: os.Getpid(), Intent: "archive",
	})
	if lerr != nil {
		// Contended, or locking is unsupported on this platform: skip. This
		// is opportunistic maintenance, not a correctness-bearing step —
		// there will be another write, and another chance, soon.
		return 0, nil
	}
	defer lock.Release()

	all, err := Posts()
	if err != nil || len(all) == 0 {
		return 0, err
	}
	window := RetentionWindow()
	cutoff := now.Add(-window)
	readers := boardReaderStates()
	audiences := newAudienceSnapshot()

	var toArchive []Post
	for _, p := range all {
		at, perr := time.Parse(time.RFC3339, p.At)
		if perr != nil || !at.Before(cutoff) {
			break
		}
		if !canArchivePost(p, readers, now, window, audiences) {
			break
		}
		toArchive = append(toArchive, p)
	}
	if len(toArchive) == 0 {
		return 0, nil
	}

	if err := appendToArchive(toArchive); err != nil {
		return 0, err
	}
	remaining := all[len(toArchive):]
	if err := rewritePosts(remaining); err != nil {
		return 0, err
	}
	if err := writeArchiveWatermark(toArchive[len(toArchive)-1].Seq); err != nil {
		return 0, err
	}
	return len(toArchive), nil
}

// appendToArchive writes posts into their monthly files, grouped so a
// rotation spanning several months (the first rotation after this feature
// ships, on an old board) does not interleave them.
func appendToArchive(posts []Post) error {
	if err := os.MkdirAll(archiveDir(), 0o755); err != nil {
		return err
	}
	byMonth := map[string][]Post{}
	var order []string
	for _, p := range posts {
		key := archiveMonthKey(p)
		if _, ok := byMonth[key]; !ok {
			order = append(order, key)
		}
		byMonth[key] = append(byMonth[key], p)
	}
	for _, key := range order {
		path := filepath.Join(archiveDir(), key+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		for _, p := range byMonth[key] {
			line, merr := json.Marshal(p)
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

// rewritePosts replaces posts.jsonl with exactly these posts, in order, seq
// untouched. Called only while holding boardLockPath, so this can never race
// a concurrent append.
func rewritePosts(posts []Post) error {
	var b strings.Builder
	for _, p := range posts {
		line, err := json.Marshal(p)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := postsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, postsPath())
}

// shouldAttemptRotation is the cheap check (one stat) that keeps the
// expensive one (a full board scan) off the common path.
func shouldAttemptRotation(now time.Time) bool {
	info, err := os.Stat(rotationMarkerPath())
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) >= rotationCheckInterval
}

func touchRotationMarker() {
	p := rotationMarkerPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	now := time.Now()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		_ = os.WriteFile(p, nil, 0o644)
		return
	}
	_ = os.Chtimes(p, now, now)
}

// rotateBoardOpportunistic is what PostMessageSeq calls after every append:
// throttled, best-effort, and never allowed to fail the post that triggered
// it.
func rotateBoardOpportunistic() {
	now := time.Now()
	if !shouldAttemptRotation(now) {
		return
	}
	touchRotationMarker()
	_, _ = RotateBoard(now)
}
