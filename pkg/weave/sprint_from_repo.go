package weave

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/ref"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// A sprint travels in the repo, as the frontmatter of its stories.
//
// The board is per host — each user/host runs its own numbers, keeps its own
// lease, thread and goals — and nothing syncs it: git is the only carrier
// between hosts. What git carries is the stories, and every story names its
// sprint by uuid (`sprint_id`), title (`sprint_title`) and the filer's local
// number (`sprint`). That is enough for a host that has no such card to make
// one: same uuid, the filer's number if it is free here and the next one if
// not, the title, the checkout as its story root. Two hosts then hold two
// INSTANCES of one sprint — the uuid says so — that agree on exactly what git
// carries (which stories, in which state) and on nothing else, which is the
// point: sessions and cards are local, coordination between people happens
// outside, keyed by the uuid.
//
// The card is created on the first verb that names it and the next verb finds
// it by uuid, so there is no adopt step and nothing to undo. A host-local
// board write is not a write to the checkout (D13 holds). The number may
// differ from the one in the frontmatter — that is the seq being a label
// scoped to its host, not a defect to reconcile.

// repoSprint is one sprint as the checkout's stories describe it.
type repoSprint struct {
	UUID    string
	Title   string
	Seq     int64 // the number most stories carry — the filer's local label
	Stories int
	Root    string
}

// repoSprints groups the checkout's stories by sprint_id. Stories without one
// belong to nothing a foreign host can name and are skipped.
func repoSprints(root string) ([]repoSprint, error) {
	items, err := todopkg.List(todopkg.RepoStore(root), "")
	if err != nil {
		return nil, err
	}
	type acc struct {
		titles map[string]int
		seqs   map[int64]int
		n      int
	}
	byUUID := map[string]*acc{}
	for _, it := range items {
		id := strings.ToLower(strings.TrimSpace(it.SprintID))
		if id == "" {
			continue
		}
		a := byUUID[id]
		if a == nil {
			a = &acc{titles: map[string]int{}, seqs: map[int64]int{}}
			byUUID[id] = a
		}
		a.n++
		if t := strings.TrimSpace(it.SprintTitle); t != "" {
			a.titles[t]++
		}
		if it.Sprint > 0 {
			a.seqs[it.Sprint]++
		}
	}
	out := make([]repoSprint, 0, len(byUUID))
	for id, a := range byUUID {
		rs := repoSprint{UUID: id, Stories: a.n, Root: root}
		best := 0
		for t, n := range a.titles {
			if n > best || (n == best && t < rs.Title) {
				rs.Title, best = t, n
			}
		}
		best = 0
		for s, n := range a.seqs {
			if n > best || (n == best && s < rs.Seq) {
				rs.Seq, best = s, n
			}
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].UUID < out[j].UUID
	})
	return out, nil
}

// matchRepoSprint picks the checkout's sprint a handle names: a uuid or its
// prefix, a bare number (the label the stories carry), or the slug of the
// title. Ambiguity is named, never guessed.
func matchRepoSprint(cands []repoSprint, handle string) (*repoSprint, error) {
	_, local := ref.SplitScope(strings.TrimSpace(handle))
	local = strings.TrimPrefix(local, "#")
	var hits []repoSprint
	switch ref.ShapeOf(local) {
	case ref.ShapeSeq:
		n, _ := strconv.ParseInt(local, 10, 64)
		for _, c := range cands {
			if c.Seq == n {
				hits = append(hits, c)
			}
		}
	case ref.ShapeUID:
		want := strings.ReplaceAll(strings.ToLower(local), "-", "")
		for _, c := range cands {
			if strings.HasPrefix(strings.ReplaceAll(c.UUID, "-", ""), want) {
				hits = append(hits, c)
			}
		}
	default:
		for _, c := range cands {
			if kb.Slugify(c.Title) == local {
				hits = append(hits, c)
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return &hits[0], nil
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, fmt.Sprintf("%s (#%d there, %q)", h.UUID, h.Seq, h.Title))
	}
	return nil, fmt.Errorf("sprint:%s names %d sprints in this checkout's stories — use the uuid: %s", handle, len(hits), strings.Join(names, "; "))
}

// notOnThisHost reports whether a resolver error means "no such card here"
// rather than a malformed handle or a card that lives under another scope.
func notOnThisHost(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// ensureSprintFromRepo answers a handle with this host's card, creating it
// from the checkout's stories when this host has none. created reports the
// second case so the caller can say so once.
func ensureSprintFromRepo(dir, handle string) (s *weaveStory, created bool, err error) {
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, false, err
	}
	s, ferr := findSprintByHandle(q, handle)
	if ferr == nil {
		return s, false, nil
	}
	if !notOnThisHost(ferr) {
		return nil, false, ferr
	}
	root, rerr := normalizeStoryRoot("")
	if rerr != nil {
		return nil, false, ferr
	}
	cands, cerr := repoSprints(root)
	if cerr != nil {
		return nil, false, fmt.Errorf("%w (and the stories in %s could not be read: %v)", ferr, root, cerr)
	}
	cand, merr := matchRepoSprint(cands, handle)
	if merr != nil {
		return nil, false, merr
	}
	if cand == nil {
		return nil, false, fmt.Errorf("%w, and no story in %s names it (a story carries its sprint as sprint_id in docs/todo/ frontmatter — pull, or check the uuid)", ferr, root)
	}
	lerr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		// Re-check under the lock: a concurrent verb may have created it.
		for _, have := range q.Stories {
			if strings.EqualFold(have.UUID, cand.UUID) {
				s = have
				return nil
			}
		}
		if q.NextStoryID == 0 {
			q.NextStoryID = 1
		}
		id := cand.Seq
		if id <= 0 || findWeaveStory(q, id) != nil {
			id = q.NextStoryID
		}
		if q.NextStoryID <= id {
			q.NextStoryID = id + 1
		}
		title := cand.Title
		if title == "" {
			title = "sprint " + cand.UUID
		}
		now := time.Now().UTC()
		s = &weaveStory{
			ID: id, UUID: cand.UUID, Title: title, Column: "backlog",
			StoryRoots: []string{root},
			Execution:  sprintExecution{PriorityFirst: true},
			Created:    now, UpdatedAt: now,
		}
		if cand.Seq > 0 {
			plan := filepath.Join("docs", fmt.Sprintf("sprint-%d-master-execution-plan.md", cand.Seq))
			if st, err := os.Stat(filepath.Join(root, plan)); err == nil && !st.IsDir() {
				s.SpecRef = plan
			}
		}
		weaveStoryAppend(s, "conductor", kindStage, fmt.Sprintf("created from the checkout %s: %d stories carry sprint_id %s (filed as #%d there)", root, cand.Stories, cand.UUID, cand.Seq))
		q.Stories = append(q.Stories, s)
		mintSprintHandles(q)
		created = true
		return nil
	})
	if lerr != nil {
		return nil, false, lerr
	}
	return s, created, nil
}

// sprintArg resolves a verb's <sprint> argument — a number, uuid, slug or
// ancestral path — to this host's card, creating it from the checkout when
// this host has none, and says so once on stderr. The verbs a host reaches
// for first (show, take, start, next, claim, track, tick, move) go through
// it; every other verb finds the card that one of these created.
func sprintArg(cmd *cobra.Command, mode weavecli.OutputMode, op, arg string) (int64, error) {
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return 0, err
	}
	s, created, err := ensureSprintFromRepo(dir, arg)
	if err != nil {
		return 0, ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
	}
	if created {
		announceSprintFromRepo(cmd.ErrOrStderr(), s)
	}
	return s.ID, nil
}

func announceSprintFromRepo(w io.Writer, s *weaveStory) {
	root := ""
	if len(s.StoryRoots) > 0 {
		root = s.StoryRoots[0]
	}
	fmt.Fprintf(w, "sprint: created #%d on this host from the stories in %s — %s (uuid %s; the number is this host's, the uuid is the sprint)\n", s.ID, root, s.Title, s.UUID)
}

// repoSprintsNotOnBoard lists the sprints the current checkout's stories name
// that this host has no card for — what `bashy sprint` shows under the board
// so a fresh clone sees the team's sprints before touching any of them.
func repoSprintsNotOnBoard(q *weaveQueue) []repoSprint {
	root, err := normalizeStoryRoot("")
	if err != nil {
		return nil
	}
	cands, err := repoSprints(root)
	if err != nil {
		return nil
	}
	have := map[string]bool{}
	for _, s := range q.Stories {
		have[strings.ToLower(s.UUID)] = true
	}
	var out []repoSprint
	for _, c := range cands {
		if !have[c.UUID] {
			out = append(out, c)
		}
	}
	return out
}

// renderSprintsFromRepo prints the board's "in this checkout, not on this
// host" section — nothing when there is nothing to say.
func renderSprintsFromRepo(w io.Writer, cands []repoSprint) {
	if len(cands) == 0 {
		return
	}
	fmt.Fprintf(w, "\nin this checkout, not on this host (%d) — `sprint show <uuid>` creates the card here:\n", len(cands))
	for _, c := range cands {
		title := c.Title
		if title == "" {
			title = "(no sprint_title in the stories)"
		}
		fmt.Fprintf(w, "  %s  %s  (%d stories; filed as #%d there)\n", c.UUID, weaveTruncate(title, 52), c.Stories, c.Seq)
	}
}
