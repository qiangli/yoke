package weave

import (
	"strings"

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
