package weave

// Story close and `sprint end` resource reclamation (Sprint 224 S4 + Sprint 652 #3286).
//
// Anything a closed story or an ended sprint still holds — a `story:*`
// announcement, a `--story`-linked lease/project claim, a settled weave
// workspace, or a `sprint grade` checkout — is a resource leak. Every closure
// path (`sprint accept`, `todo done`, `todo status ... wontfix`, moving a story
// off a sprint, and `sprint end`) funnels through reclaimStoryResources,
// outside the sprint board lock, using weavePruneOwnedRunWithOptions so dirty
// trees and unique unmerged/unsalvaged commits are refused, never deleted.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func validateRetainFlags(retain bool, reason, defaultOwner string) (owner, trimmedReason string, err error) {
	trimmedReason = strings.TrimSpace(reason)
	if !retain && trimmedReason != "" {
		return "", "", fmt.Errorf("--retain-reason requires --retain")
	}
	if retain && trimmedReason == "" {
		return "", "", fmt.Errorf("--retain requires --retain-reason")
	}
	if !retain {
		return "", "", nil
	}
	owner = strings.TrimSpace(defaultOwner)
	if owner == "" {
		owner = strings.TrimSpace(weaveConductorName(""))
	}
	if owner == "" {
		owner = strings.TrimSpace(coord.Self().Name)
	}
	if owner == "" {
		owner = "operator"
	}
	return owner, trimmedReason, nil
}

type matchedStoryRun struct {
	queueDir   string
	repo       string
	id         int64
	born       time.Time
	checkBirth bool
	item       *weaveItem
}

func weaveRunMatchesStory(queueRoot string, link *sprintRun, it *weaveItem, sprintID int64, sprintUUID, storyRoot, storyID string, storyWeaveID int64) bool {
	if it == nil || storyID == "" {
		return false
	}
	if link != nil && link.Story != "" {
		return link.Story == storyID
	}
	if it.Register != "" {
		if it.Register == storyID {
			return true
		}
		return false
	}
	for _, ws := range it.WorkerStories {
		if ws.ID != storyID {
			continue
		}
		if sprintUUID != "" && ws.SprintID != "" {
			if strings.EqualFold(ws.SprintID, sprintUUID) {
				return true
			}
			continue
		}
		if sprintID != 0 && ws.Sprint != 0 && ws.Sprint != sprintID {
			continue
		}
		return true
	}
	if storyWeaveID > 0 && storyWeaveID == it.ID && storyRoot != "" && queueRoot != "" &&
		filepath.Clean(queueRoot) == filepath.Clean(storyRoot) {
		return true
	}
	return false
}

func collectStoryRuns(s *weaveStory, sprintID int64, sprintUUID, storyRoot, storyID string, storyWeaveID int64) []matchedStoryRun {
	type runKey struct {
		dir string
		id  int64
	}
	seen := map[runKey]bool{}
	var out []matchedStoryRun

	addFromSprint := func(card *weaveStory) {
		if card == nil {
			return
		}
		for _, run := range card.Runs {
			dir, err := weaveQueueDirForSprintRun(run)
			if err != nil {
				continue
			}
			cleanDir := filepath.Clean(dir)
			q, err := loadWeaveQueue(dir)
			if err != nil {
				continue
			}
			it := findWeaveItem(q, run.ID)
			if it == nil {
				continue
			}
			if !run.Born.IsZero() && !run.Born.Equal(it.Created) {
				continue
			}
			if storyID != "" && !weaveRunMatchesStory(q.Root, &run, it, card.ID, card.UUID, storyRoot, storyID, storyWeaveID) {
				continue
			}
			k := runKey{dir: cleanDir, id: run.ID}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, matchedStoryRun{
				queueDir:   dir,
				repo:       run.Repo,
				id:         run.ID,
				born:       run.Born,
				checkBirth: !run.Born.IsZero(),
				item:       it,
			})
		}
	}

	addFromSprint(s)

	// Also inspect the story repository's own weave queue for runs linked by
	// Register, WorkerStories, or story.Weave that were never `sprint link`ed.
	var roots []string
	if storyRoot != "" {
		roots = append(roots, storyRoot)
	} else if s != nil && storyID == "" {
		roots = append(roots, sprintDeclaredStoryRoots(s)...)
	}
	for _, root := range roots {
		dir, err := weaveQueueDir(root)
		if err != nil {
			continue
		}
		cleanDir := filepath.Clean(dir)
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		repo := filepath.Base(root)
		for _, it := range q.Items {
			k := runKey{dir: cleanDir, id: it.ID}
			if seen[k] {
				continue
			}
			if storyID != "" {
				if !weaveRunMatchesStory(root, nil, it, sprintID, sprintUUID, storyRoot, storyID, storyWeaveID) {
					continue
				}
			} else if s != nil {
				matched := false
				for _, ws := range it.WorkerStories {
					if (s.UUID != "" && ws.SprintID != "" && strings.EqualFold(s.UUID, ws.SprintID)) ||
						(ws.SprintID == "" && ws.Sprint == s.ID) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			} else {
				continue
			}
			seen[k] = true
			out = append(out, matchedStoryRun{
				queueDir: dir,
				repo:     repo,
				id:       it.ID,
				item:     it,
			})
		}
	}
	return out
}

// sprintUndisposedRuns names every linked run whose work has no decision yet,
// each with the verb that makes one. A run that is terminal and provably
// settled (merged, empty, or salvaged) needs no explicit word — the teardown
// records it — so old rows from before the field existed do not block a close.
func sprintUndisposedRuns(s *weaveStory) []string {
	return storyUndisposedRuns(s, "", "", 0)
}

func storyUndisposedRuns(s *weaveStory, storyRoot, storyID string, storyWeaveID int64) []string {
	var sprintID int64
	var sprintUUID string
	if s != nil {
		sprintID = s.ID
		sprintUUID = s.UUID
	}
	var out []string
	var runs []matchedStoryRun
	if storyID == "" && s != nil {
		// Preserve exact s.Runs scoping for sprint-wide checks so unlinked
		// sibling runs in shared queues behave identically to Sprint 224 S4.
		for _, run := range s.Runs {
			dir, err := weaveQueueDirForSprintRun(run)
			if err != nil {
				continue
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				continue
			}
			it := findWeaveItem(q, run.ID)
			if it == nil {
				continue
			}
			runs = append(runs, matchedStoryRun{queueDir: dir, repo: run.Repo, id: run.ID, item: it})
		}
	} else {
		runs = collectStoryRuns(s, sprintID, sprintUUID, storyRoot, storyID, storyWeaveID)
	}
	for _, r := range runs {
		root, ok := weaveRepoRootForQueue(r.queueDir)
		if !ok {
			continue
		}
		it := r.item
		if !isTerminalState(it.State) {
			out = append(out, fmt.Sprintf("run %s#%d is %s — let it finish, `bashy weave kill %d`, or `bashy weave abandon %d --disposition superseded`",
				r.repo, r.id, it.State, r.id, r.id))
			continue
		}
		if _, settled := weaveItemSettled(root, weaveBaseBranch(root), it); settled {
			if it.Workspace != "" {
				if st, err := os.Stat(it.Workspace); err == nil && st.IsDir() {
					if dirty, dirtyFiles, untracked := weaveMeasureDirtiness(it.Workspace); dirty || untracked > 0 {
						out = append(out, fmt.Sprintf("run %s#%d workspace has %d uncommitted/untracked file(s) — commit, salvage, or `bashy weave abandon %d --force`",
							r.repo, r.id, dirtyFiles+untracked, r.id))
					}
				}
			}
			continue
		}
		switch it.State {
		case "submitted":
			out = append(out, fmt.Sprintf("run %s#%d is submitted and undecided — accept: `bashy weave pull %d`; decline: `bashy weave abandon %d --disposition rejected|superseded --reason '<why>'`",
				r.repo, r.id, r.id, r.id))
		default:
			out = append(out, fmt.Sprintf("run %s#%d (%s) holds unmerged work — keep: `bashy weave salvage %d`; decline: `bashy weave abandon %d --disposition rejected|superseded --reason '<why>'`",
				r.repo, r.id, it.State, r.id, r.id))
		}
	}
	sort.Strings(out)
	return out
}

type storyReclaimOptions struct {
	Sprint           *weaveStory
	SprintID         int64
	SprintUUID       string
	SprintScopedOnly bool
	StoryRoot        string
	StoryID          string
	StoryWeaveID     int64
	Actor            string
	ClosingVerb      string
	MaxEpochByKey    map[string]uint64
}

// preflightAttachedStoryClaims refuses story close or sprint end while any live
// kernel-locked ModeAttached claim still serves the target story or sprint.
func preflightAttachedStoryClaims(s *weaveStory, sprintID int64, storyID string, sprintScopedOnly bool) error {
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		return err
	}
	sprintStoryIDs := sprintCollectStoryIDs(s)
	var active []string
	for _, c := range claims {
		if c.Mode != coord.ModeAttached {
			continue
		}
		matched := false
		if storyID != "" {
			matched = storyClaimMatchesStory(c, sprintID, storyID, sprintScopedOnly)
		} else {
			matched = storyClaimMatchesSprint(c, sprintID, sprintStoryIDs)
		}
		if matched {
			holder := c.Holder.Name
			if holder == "" {
				holder = "unattributed"
			}
			active = append(active, fmt.Sprintf("%s held by %s (pid %d)", c.Key(), holder, c.PID))
		}
	}
	if len(active) == 0 {
		return nil
	}
	sort.Strings(active)
	return fmt.Errorf("live attached claim(s) still active — wait for the child process to finish before closing:\n  %s",
		strings.Join(active, "\n  "))
}

// snapshotMatchingClaimEpochs records the current fencing epoch of every claim
// matching the target story/sprint right before closure commits, so a claim
// newly acquired after a concurrent reopen or move is never reclaimed by the
// finishing close pass.
func snapshotMatchingClaimEpochs(s *weaveStory, sprintID int64, storyID string, sprintScopedOnly bool) map[string]uint64 {
	out := map[string]uint64{}
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		return out
	}
	sprintStoryIDs := sprintCollectStoryIDs(s)
	for _, c := range claims {
		matched := false
		if storyID != "" {
			matched = storyClaimMatchesStory(c, sprintID, storyID, sprintScopedOnly)
		} else {
			matched = storyClaimMatchesSprint(c, sprintID, sprintStoryIDs)
		}
		if matched {
			out[c.Key()] = c.Epoch
		}
	}
	return out
}

func closureStillHolds(opts storyReclaimOptions) bool {
	if opts.StoryID != "" {
		if opts.StoryRoot != "" {
			if it, err := todopkg.ResolveRef(todopkg.RepoStore(opts.StoryRoot), opts.StoryID); err == nil && it != nil {
				if opts.SprintScopedOnly {
					if opts.Sprint != nil {
						return !storyBelongsToSprint(it, opts.Sprint)
					}
					return opts.SprintID > 0 && it.Sprint != opts.SprintID
				}
				return todopkg.IsClosed(it.Status)
			}
		}
		return true
	}
	if opts.SprintID > 0 {
		if dir, err := sprintStoreDir(); err == nil {
			if q, err := loadWeaveQueue(dir); err == nil {
				if card := findWeaveStory(q, opts.SprintID); card != nil {
					// Pre-lock sprint end runs while card is still open, and post-lock card is done;
					// only abort if the card disappeared.
					return true
				}
			}
		}
	}
	return true
}

func parseStoryClaimTag(raw string) (sprint int64, storyID string, ok bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "todo:")
	if raw == "" {
		return 0, "", false
	}
	if left, right, hasSlash := strings.Cut(raw, "/"); hasSlash {
		seq, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(left), "#"), 10, 64)
		id := strings.TrimSpace(right)
		if err != nil || seq <= 0 || id == "" {
			return 0, "", false
		}
		return seq, id, true
	}
	return 0, raw, true
}

func storyIDMatches(tagID, fullStoryID string) bool {
	if tagID == "" || fullStoryID == "" {
		return false
	}
	if strings.EqualFold(tagID, fullStoryID) {
		return true
	}
	// Support unambiguous short prefixes (e.g. --story 652/a52829ea) of >=8 chars.
	return len(tagID) >= 8 && len(fullStoryID) > len(tagID) &&
		strings.HasPrefix(strings.ToLower(fullStoryID), strings.ToLower(tagID))
}

func storyClaimMatchesStory(c *coord.Claim, sprintID int64, storyID string, sprintScopedOnly bool) bool {
	if c == nil || storyID == "" {
		return false
	}
	if c.Kind == storyKind {
		seq, id, ok := parseStoryClaimTag(c.Resource)
		if !ok || !storyIDMatches(id, storyID) {
			return false
		}
		if sprintScopedOnly && sprintID > 0 {
			return seq == sprintID
		}
		return true
	}
	if strings.TrimSpace(c.Story) == "" {
		return false
	}
	seq, id, ok := parseStoryClaimTag(c.Story)
	if !ok || !storyIDMatches(id, storyID) {
		return false
	}
	if sprintScopedOnly {
		return sprintID > 0 && seq == sprintID
	}
	if seq > 0 && sprintID > 0 && seq != sprintID {
		return false
	}
	return true
}

func storyClaimMatchesSprint(c *coord.Claim, sprintID int64, sprintStoryIDs map[string]bool) bool {
	if c == nil || sprintID <= 0 {
		return false
	}
	if c.Kind == storyKind {
		seq, _, ok := parseStoryClaimTag(c.Resource)
		return ok && seq == sprintID
	}
	if strings.TrimSpace(c.Story) == "" {
		return false
	}
	seq, id, ok := parseStoryClaimTag(c.Story)
	if !ok {
		return false
	}
	if seq == sprintID {
		return true
	}
	if seq == 0 {
		for sid := range sprintStoryIDs {
			if storyIDMatches(id, sid) {
				return true
			}
		}
	}
	return false
}

func reclaimRecordTimeline(sprintID int64, storyID, actor, closingVerb, detail string) {
	who := strings.TrimSpace(actor)
	if who == "" {
		who = strings.TrimSpace(weaveConductorName(""))
	}
	if who == "" {
		who = "system"
	}
	typ := room.EventTodo
	target := "todo:" + shortSprintStoryID(storyID)
	if sprintID > 0 {
		typ = room.EventSprint
		target = fmt.Sprintf("sprint:#%d", sprintID)
	}
	room.Record(typ, who, target, "reclaim", fmt.Sprintf("%s: %s", closingVerb, detail))
}

func reclaimGradeDir(baseGradeRoot, dirPath, repo string) []sprintPruneAction {
	info, err := os.Lstat(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []sprintPruneAction{{Kind: "grade", Repo: repo, Target: dirPath, Err: err.Error()}}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return []sprintPruneAction{{Kind: "grade", Repo: repo, Target: dirPath, Err: "grade path is a symlink; left alone"}}
	}
	if err := weaveContainedArtifact(baseGradeRoot, dirPath); err != nil {
		return []sprintPruneAction{{Kind: "grade", Repo: repo, Target: dirPath, Err: err.Error()}}
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return []sprintPruneAction{{Kind: "grade", Repo: repo, Target: dirPath, Err: err.Error()}}
	}
	var acts []sprintPruneAction
	for _, entry := range entries {
		child := filepath.Join(dirPath, entry.Name())
		cinfo, cerr := os.Lstat(child)
		if cerr != nil {
			continue
		}
		if cinfo.Mode()&os.ModeSymlink != 0 {
			acts = append(acts, sprintPruneAction{Kind: "grade", Repo: repo, Target: child, Err: "grade entry is a symlink; left alone"})
			continue
		}
		bytes, berr := weaveArtifactBytes(child)
		bounded := errors.Is(berr, errWeaveArtifactScanBound)
		if berr != nil && !bounded {
			acts = append(acts, sprintPruneAction{Kind: "grade", Repo: repo, Target: child, Err: berr.Error()})
			continue
		}
		a := sprintPruneAction{
			Kind:          "grade",
			Repo:          repo,
			Target:        child,
			ExpectedBytes: bytes,
			ByteKind:      "apparent_regular_file_bytes",
		}
		if rerr := os.RemoveAll(child); rerr != nil {
			a.Err = rerr.Error()
		} else {
			a.Done = true
			a.ActualBytes = bytes
			a.BytesComplete = !bounded
		}
		acts = append(acts, a)
	}
	_ = os.Remove(dirPath)
	_ = os.Remove(filepath.Dir(dirPath))
	return acts
}

func sprintCollectStoryIDs(s *weaveStory) map[string]bool {
	ids := map[string]bool{}
	if s == nil {
		return ids
	}
	for _, g := range s.Goal {
		for _, st := range g.Stories {
			if st.ID != "" {
				ids[st.ID] = true
			}
		}
	}
	for _, r := range s.Runs {
		if r.Story != "" {
			ids[r.Story] = true
		}
	}
	for _, root := range sprintDeclaredStoryRoots(s) {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			continue
		}
		for _, it := range items {
			if storyBelongsToSprint(it, s) {
				ids[it.ID] = true
			}
		}
	}
	return ids
}

// reclaimStoryResources is the ONE lifecycle reclaim function shared by story
// close (`sprint accept`, `todo done`, `todo wontfix`, `todo edit --sprint`)
// and `sprint end`.
func reclaimStoryResources(opts storyReclaimOptions) ([]sprintPruneAction, error) {
	if opts.Sprint != nil {
		if opts.SprintID == 0 {
			opts.SprintID = opts.Sprint.ID
		}
		if opts.SprintUUID == "" {
			opts.SprintUUID = opts.Sprint.UUID
		}
	} else if opts.SprintID > 0 || opts.SprintUUID != "" {
		if dir, err := sprintStoreDir(); err == nil {
			if q, err := loadWeaveQueue(dir); err == nil {
				for _, card := range q.Stories {
					if (opts.SprintUUID != "" && card.UUID != "" && strings.EqualFold(opts.SprintUUID, card.UUID)) ||
						(opts.SprintUUID == "" && card.ID == opts.SprintID) {
						opts.Sprint = card
						opts.SprintID = card.ID
						opts.SprintUUID = card.UUID
						break
					}
				}
			}
		}
	}
	verb := strings.TrimSpace(opts.ClosingVerb)
	if verb == "" {
		verb = "close"
	}

	var acts []sprintPruneAction
	sprintStoryIDs := sprintCollectStoryIDs(opts.Sprint)

	// 1. Reclaim story announcements and story-tagged leases in coord.
	reclaimedClaims, claimErr := coord.ReclaimClaims(coord.DefaultDir(), func(c *coord.Claim) (string, bool) {
		if !closureStillHolds(opts) {
			return "", false
		}
		if opts.MaxEpochByKey != nil {
			maxEpoch, seen := opts.MaxEpochByKey[c.Key()]
			if !seen || c.Epoch > maxEpoch {
				return "", false
			}
		}
		if opts.StoryID != "" {
			if storyClaimMatchesStory(c, opts.SprintID, opts.StoryID, opts.SprintScopedOnly) {
				return verb, true
			}
			return "", false
		}
		if storyClaimMatchesSprint(c, opts.SprintID, sprintStoryIDs) {
			return verb, true
		}
		return "", false
	})
	for _, rc := range reclaimedClaims {
		acts = append(acts, sprintPruneAction{
			Kind:          "claim",
			Target:        rc.Key,
			Done:          true,
			BytesComplete: true,
		})
		holder := rc.Claim.Holder.Name
		if holder == "" {
			holder = "unattributed"
		}
		detail := fmt.Sprintf("released claim %s (holder %s)", rc.Key, holder)
		if opts.StoryID != "" {
			detail += fmt.Sprintf(" for story %s", shortSprintStoryID(opts.StoryID))
		}
		reclaimRecordTimeline(opts.SprintID, opts.StoryID, opts.Actor, verb, detail)
	}

	// 2. Tear down settled weave runs (ending --retain; refusing dirty/unsettled).
	var runs []matchedStoryRun
	if opts.StoryID == "" && opts.Sprint != nil {
		for _, run := range opts.Sprint.Runs {
			dir, err := weaveQueueDirForSprintRun(run)
			if err != nil {
				continue
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				continue
			}
			it := findWeaveItem(q, run.ID)
			if it == nil {
				continue
			}
			runs = append(runs, matchedStoryRun{
				queueDir:   dir,
				repo:       run.Repo,
				id:         run.ID,
				born:       run.Born,
				checkBirth: true,
				item:       it,
			})
		}
	} else {
		runs = collectStoryRuns(opts.Sprint, opts.SprintID, opts.SprintUUID, opts.StoryRoot, opts.StoryID, opts.StoryWeaveID)
	}

	runLabels := map[string]string{} // "<repo>#<id>" -> repo
	for _, r := range runs {
		runLabels[fmt.Sprintf("%s#%d", r.repo, r.id)] = r.repo
		root, ok := weaveRepoRootForQueue(r.queueDir)
		if !ok {
			continue
		}
		if !isTerminalState(r.item.State) {
			acts = append(acts, sprintPruneAction{
				Kind:   "workspace",
				Repo:   r.repo,
				Target: fmt.Sprintf("%s#%d", r.repo, r.id),
				Err:    fmt.Sprintf("run %s#%d is %s; refused deletion", r.repo, r.id, r.item.State),
			})
			continue
		}
		if _, settled := weaveItemSettled(root, weaveBaseBranch(root), r.item); !settled {
			target := r.item.Workspace
			if target == "" {
				target = fmt.Sprintf("%s#%d", r.repo, r.id)
			}
			acts = append(acts, sprintPruneAction{
				Kind:   "workspace",
				Repo:   r.repo,
				Target: target,
				Err:    fmt.Sprintf("run %s#%d holds unpushed/unmerged unique commits; refused deletion", r.repo, r.id),
			})
			continue
		}
		pruned := weavePruneOwnedRunWithOptions(r.queueDir, r.id, r.repo, weavePruneOptions{
			EndRetain:     true,
			ExpectedBirth: r.born,
		}, r.checkBirth)
		for _, a := range pruned {
			acts = append(acts, a)
			if a.Done && a.Err == "" && a.Kind != "lock" {
				detail := fmt.Sprintf("deleted %s %s", a.Kind, a.Target)
				if opts.StoryID != "" {
					detail += fmt.Sprintf(" for story %s", shortSprintStoryID(opts.StoryID))
				}
				reclaimRecordTimeline(opts.SprintID, opts.StoryID, opts.Actor, verb, detail)
			}
		}
	}

	// 3. Delete temporary and retained `sprint grade` checkouts.
	if opts.SprintID > 0 {
		if gradeRoot, gerr := sprintGradeRootDir(); gerr == nil {
			if opts.StoryID == "" {
				if sprintDir, serr := sprintGradeSprintDir(opts.SprintID); serr == nil {
					if entries, rerr := os.ReadDir(sprintDir); rerr == nil {
						for _, entry := range entries {
							hashDir := filepath.Join(sprintDir, entry.Name())
							gacts := reclaimGradeDir(gradeRoot, hashDir, "")
							for _, a := range gacts {
								acts = append(acts, a)
								if a.Done && a.Err == "" {
									reclaimRecordTimeline(opts.SprintID, "", opts.Actor, verb, fmt.Sprintf("deleted grade checkout %s", a.Target))
								}
							}
						}
						_ = os.Remove(sprintDir)
					}
				}
			} else {
				for label, repo := range runLabels {
					runDir, rerr := sprintGradeRunDir(opts.SprintID, label)
					if rerr != nil {
						continue
					}
					gacts := reclaimGradeDir(gradeRoot, runDir, repo)
					for _, a := range gacts {
						acts = append(acts, a)
						if a.Done && a.Err == "" {
							reclaimRecordTimeline(opts.SprintID, opts.StoryID, opts.Actor, verb,
								fmt.Sprintf("deleted grade checkout %s for story %s", a.Target, shortSprintStoryID(opts.StoryID)))
						}
					}
				}
			}
		}
	}

	return acts, claimErr
}

// sprintResidual is what would still be sprint-owned after the reclaim: an
// artifact the plan can still see, a branch name a run still owns, a leftover
// grade checkout, or a recorded cleanup failure. Empty means residual = 0.
func sprintResidual(s *weaveStory) []string {
	seen := map[string]bool{}
	var out []string
	add := func(line string) {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	for _, a := range sprintPlanRunArtifacts(s) {
		line := fmt.Sprintf("%s %s", a.Kind, a.Target)
		if a.Err != "" {
			line += " (" + a.Err + ")"
		}
		add(line)
	}
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		it := findWeaveItem(q, run.ID)
		if it == nil {
			continue
		}
		if it.CleanupError != "" {
			add(fmt.Sprintf("run %s#%d cleanup failed: %s", run.Repo, run.ID, it.CleanupError))
		}
		root, ok := weaveRepoRootForQueue(dir)
		if !ok {
			continue
		}
		for _, name := range weaveRunBranchNames(it) {
			if _, exists := gitBranchTip(root, name); exists {
				add(fmt.Sprintf("branch %s in %s", name, root))
			}
		}
		for _, kind := range []string{"cache", "agent-data"} {
			if p := weaveRunArtifactPath(dir, it, kind); p != "" {
				if _, err := os.Lstat(p); err == nil {
					add(fmt.Sprintf("%s %s", kind, p))
				}
			}
		}
	}
	if s != nil && s.ID > 0 {
		if sprintDir, err := sprintGradeSprintDir(s.ID); err == nil {
			if entries, err := os.ReadDir(sprintDir); err == nil {
				for _, e := range entries {
					add(fmt.Sprintf("grade %s", filepath.Join(sprintDir, e.Name())))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// sprintReclaimSummary renders what the reclaim did, by artifact kind, with
// apparent bytes — the number the operator asked for and the one that must
// never be read as physical free space.
func sprintReclaimSummary(actions []sprintPruneAction) (string, []string) {
	type tally struct {
		n     int
		bytes uint64
	}
	kinds := map[string]*tally{}
	var failures []string
	for _, a := range actions {
		if a.Err != "" {
			failures = append(failures, fmt.Sprintf("%s %s: %s", a.Kind, a.Target, a.Err))
			continue
		}
		if !a.Done {
			continue
		}
		t := kinds[a.Kind]
		if t == nil {
			t = &tally{}
			kinds[a.Kind] = t
		}
		t.n++
		t.bytes += a.ActualBytes
	}
	var parts []string
	for _, k := range []string{"workspace", "cache", "agent-data", "log", "socket", "lock", "branch", "grade", "claim"} {
		if t := kinds[k]; t != nil {
			if t.bytes > 0 {
				parts = append(parts, fmt.Sprintf("%d %s (%d apparent bytes)", t.n, k, t.bytes))
			} else {
				parts = append(parts, fmt.Sprintf("%d %s", t.n, k))
			}
		}
	}
	sort.Strings(failures)
	if len(parts) == 0 {
		return "nothing to reclaim", failures
	}
	return "reclaimed " + strings.Join(parts, ", "), failures
}

// weaveStoryRead is the lock-free read counterpart of runWeaveStoryMutate for
// work that must happen BEFORE the mutation takes the queue lock.
func weaveStoryRead(cmd *cobra.Command, flags *weaveOutputFlags, op string, id int64, fn func(*weaveStory)) error {
	mode := flags.mode()
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, fmt.Errorf("sprint #%d not found", id)))
	}
	fn(s)
	return nil
}

// sprintEndCleanupTally is the end-cleanup checks' passed/total, the hygiene
// input of the manager scorecard. Undisposed runs and reclaim failures refuse
// end before its mutation, so they are not counted again here; of the rest,
// repo hygiene and zero residue also refuse, and the one that never blocks —
// host artifacts `sprint prune` could still reclaim — is what can fall short.
func sprintEndCleanupTally(hy sprintHygiene, residual []string) (passed, total int) {
	for _, ok := range []bool{hy.Clean(), len(residual) == 0, len(hy.Reclaimable) == 0} {
		total++
		if ok {
			passed++
		}
	}
	return passed, total
}
