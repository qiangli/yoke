package weave

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/ref"
)

// A sprint carries the three handles every entity has (D14,
// docs/uniform-ref-addressing.md): the seq humans type on this host, a uuid
// that is the identity across hosts, and a slug for prose and the web. The
// seq is unique only within its parent — each user/host runs its own sprint
// numbers — so from another host a sprint is named by its ancestral path
// (`sprint:<user>/<host>/<seq>`) or by its uuid; never by the seq alone.
//
// Cards written before this existed have a bare seq. Missing handles are
// minted ON DEMAND at load and persisted by the next ordinary write — and
// minting is DETERMINISTIC (a name-based uuid over the card's own parentage
// and creation instant), so a read-only verb prints the same uuid every time
// without dirtying the store, and the write that follows persists exactly
// what was printed.

// sprintUUIDNamespace is the fixed namespace the name-based derivation hashes
// under; changing it would rename every not-yet-persisted card.
var sprintUUIDNamespace = uuid.MustParse("6f3b2d0e-4d1a-4f0b-9c3e-7a2d8b5e1c40")

// sprintScopePath is the sprint's ancestral path on THIS host: `<user>/<host>`.
func sprintScopePath() string {
	return principalLocalUser() + "/" + sessionHostName()
}

// mintSprintHandles fills the uuid and slug of every card that lacks them, in
// memory. Deterministic: the same card derives the same handles on every load
// until a write persists them. Slugs are unique within the store — a title
// already taken gets `-<seq>`.
func mintSprintHandles(q *weaveQueue) {
	if q == nil {
		return
	}
	scope := sprintScopePath()
	taken := map[string]int64{}
	for _, s := range q.Stories {
		if s.Slug != "" {
			taken[s.Slug] = s.ID
		}
	}
	for _, s := range q.Stories {
		if s.UUID == "" {
			name := scope + "/" + strconv.FormatInt(s.ID, 10) + "@" + strconv.FormatInt(s.Created.UTC().UnixNano(), 10)
			s.UUID = uuid.NewSHA1(sprintUUIDNamespace, []byte(name)).String()
		}
		if s.Slug == "" {
			slug := kb.Slugify(s.Title)
			if slug == "" || ref.ShapeOf(slug) != ref.ShapeSlug {
				slug = "sprint-" + strconv.FormatInt(s.ID, 10)
			}
			if owner, dup := taken[slug]; dup && owner != s.ID {
				slug = slug + "-" + strconv.FormatInt(s.ID, 10)
			}
			s.Slug = slug
			taken[slug] = s.ID
		}
	}
}

// sprintRef is the three handles and the path, for `show` and `--json`.
type sprintRef struct {
	Ref  string `json:"ref"`  // sprint:<seq> — the local spelling
	UUID string `json:"uuid"` // the identity across hosts
	Slug string `json:"slug"`
	Path string `json:"path"` // sprint:<user>/<host>/<seq> — the ancestral spelling
}

func sprintRefOf(s *weaveStory) sprintRef {
	seq := strconv.FormatInt(s.ID, 10)
	return sprintRef{
		Ref:  ref.Format(ref.Sprint, seq),
		UUID: s.UUID,
		Slug: s.Slug,
		Path: ref.Format(ref.Sprint, sprintScopePath()+"/"+seq),
	}
}

// findSprintByHandle answers any of the three handles, optionally qualified by
// the ancestral path. A path that is not this host's is not an error of
// spelling but a card that lives elsewhere: say so and point at the uuid,
// which resolves anywhere the card has travelled.
func findSprintByHandle(q *weaveQueue, handle string) (*weaveStory, error) {
	scope, local := ref.SplitScope(strings.TrimSpace(handle))
	if scope != "" && scope != sprintScopePath() {
		return nil, fmt.Errorf("sprint:%s lives under %s, not this host (%s); name it by uuid", handle, scope, sprintScopePath())
	}
	local = strings.TrimPrefix(local, "#")
	switch ref.ShapeOf(local) {
	case ref.ShapeSeq:
		n, err := strconv.ParseInt(local, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("sprint: %q is not a sprint number", local)
		}
		if s := findWeaveStory(q, n); s != nil {
			return s, nil
		}
		return nil, fmt.Errorf("sprint #%d not found", n)
	case ref.ShapeUID:
		var hits []*weaveStory
		for _, s := range q.Stories {
			if s.UUID == local || strings.HasPrefix(strings.ReplaceAll(s.UUID, "-", ""), strings.ReplaceAll(strings.ToLower(local), "-", "")) {
				hits = append(hits, s)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			return nil, fmt.Errorf("sprint:%s not found", local)
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, "#"+strconv.FormatInt(h.ID, 10))
		}
		return nil, fmt.Errorf("sprint:%s is ambiguous: %s", local, strings.Join(ids, ", "))
	default:
		for _, s := range q.Stories {
			if s.Slug == local {
				return s, nil
			}
		}
		return nil, fmt.Errorf("sprint:%s not found", local)
	}
}
