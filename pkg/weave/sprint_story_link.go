package weave

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// The sprint↔story link, both directions, keyed on the uuid.
//
// A story's frontmatter carries `sprint: <seq>` (the filer's local number, a
// label), `sprint_id: <uuid>` (the identity) and `sprint_title`. Every host
// runs its own sprint numbers, so once a repo is checked out elsewhere the
// seq may already name an unrelated card there; the uuid never does. Reading
// a sprint's stories therefore matches the uuid first and falls back to the
// seq only for stories filed before the uuid existed (nothing is rewritten on
// read — D13). Writing goes the other way: `todo add/edit --sprint N` asks the
// board, through the seam below, for the uuid and title of card N.

func init() {
	todopkg.SprintHandles = sprintHandlesForTodo
	todopkg.SprintPreflight = sprintBoardPreflight
	todopkg.SprintChanged = func(previous *issue.Issue) error {
		if previous.Sprint == 0 && previous.SprintID == "" {
			return nil
		}
		dir, err := sprintStoreDir()
		if err != nil {
			return err
		}
		return withWeaveQueueLock(dir, func(q *weaveQueue) error {
			for _, s := range q.Stories {
				if storyBelongsToSprint(previous, s) {
					sprintRetireMovedGoals(s)
				}
			}
			return nil
		})
	}
}

// sprintBoardPreflight is the pkg/todo preflight seam: it proves THIS host's
// sprint board is writable before a story move is saved anywhere. A conductor
// session confined by its sandbox (the managed-Codex profile that denies
// ~/.bashy while granting the repo) fails HERE, with the story untouched —
// instead of saving the story and then failing the board reconcile, which
// used to leave repo cards moved while the host index still said missing.
//
// The probe proves BOTH halves of the later read-modify-write: taking the
// board lock for an instant (released unmodified), and creating + removing a
// temp file the way the save's tmp+rename needs. Both halves are required:
// a lock file left over from an earlier run opens fine in a directory the
// session may no longer create files in, so the lock alone passes exactly
// when the save then fails. A lock held by another command is contention,
// not denial, and passes — the real reconcile waits its turn as before. The
// probe leaves no board state behind (no queue.json minted, temp removed),
// so a story whose save then fails leaves the board exactly as it was.
func sprintBoardPreflight() error {
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	if err := ensureWeaveQueueDirPath(dir); err != nil {
		return sprintBoardDenied(dir, err)
	}
	l, err := lockfile.TryAcquire(filepath.Join(dir, "queue.lock"), lockfile.Holder{
		Name: "sprint-preflight", PID: os.Getpid(), Intent: "prove board writable", Since: time.Now(),
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return nil
		}
		return sprintBoardDenied(dir, err)
	}
	if err := l.Release(); err != nil {
		return sprintBoardDenied(dir, err)
	}
	// The save writes queue.json via a temp file + rename; prove a temp file
	// can be created here before any other store is written.
	f, err := os.CreateTemp(dir, "queue-preflight-*.tmp")
	if err != nil {
		return sprintBoardDenied(dir, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return sprintBoardDenied(dir, err)
	}
	return nil
}

// sprintBoardDenied turns a raw lock error into the profile fix: a confined
// conductor session needs this directory (plus the repo .git dirs it
// commits) in its writable roots for the whole session.
func sprintBoardDenied(dir string, err error) error {
	return errors.Join(err,
		errors.New("sprint board "+dir+" is not writable: grant it (and the repo .git dirs this session commits) in the session's writable roots, or run the conductor outside the sandbox"))
}

// sprintHandlesForTodo is the pkg/todo seam: uuid + title of the local card
// numbered seq, or ok=false when there is no board or no such card. Never an
// error — a todo files fine with the seq alone.
func sprintHandlesForTodo(seq int64) (string, string, bool) {
	dir, err := sprintStoreDir()
	if err != nil {
		return "", "", false
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return "", "", false
	}
	s := findWeaveStory(q, seq)
	if s == nil || s.UUID == "" {
		return "", "", false
	}
	return s.UUID, s.Title, true
}

// storyBelongsToSprint is THE membership test: the uuid when the story has
// one, the seq otherwise. A story that names a different uuid is not a member
// even when its seq label happens to equal this card's number.
func storyBelongsToSprint(it *issue.Issue, s *weaveStory) bool {
	if it == nil || s == nil {
		return false
	}
	if id := strings.TrimSpace(it.SprintID); id != "" {
		return s.UUID != "" && strings.EqualFold(id, s.UUID)
	}
	return it.Sprint == s.ID
}
