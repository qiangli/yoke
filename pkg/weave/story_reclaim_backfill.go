package weave

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/prune"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func init() {
	coord.ClosedStories = weaveClosedStoryPruner{}
}

type weaveClosedStoryPruner struct{}

type closedStorySnapshot struct {
	board         *weaveQueue
	boardLoaded   bool
	boardReadable bool
	repoStores    []*issue.Store
	userStores    []*issue.Store
	cwdGitRoot    string
	cwdReadable   bool
	userReadable  bool
}

func isReadableRepoTodoRoot(root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return false
	}
	_, err := os.ReadDir(filepath.Join(root, todopkg.RepoSub))
	return err == nil || os.IsNotExist(err)
}

func buildClosedStorySnapshot() (*closedStorySnapshot, error) {
	snap := &closedStorySnapshot{
		boardReadable: true,
		userReadable:  true,
	}
	if sdir, err := sprintStoreDir(); err == nil {
		qPath := filepath.Join(sdir, "queue.json")
		if _, statErr := os.Stat(qPath); statErr == nil {
			q, lerr := loadWeaveQueue(sdir)
			if lerr != nil {
				snap.boardReadable = false
				return nil, fmt.Errorf("claim prune --stories-closed: sprint board %s is unreadable: %w", sdir, lerr)
			}
			snap.board = q
			snap.boardLoaded = true
		} else if !os.IsNotExist(statErr) {
			snap.boardReadable = false
			return nil, fmt.Errorf("claim prune --stories-closed: sprint board %s is inaccessible: %w", sdir, statErr)
		}
	}

	seenRoots := map[string]bool{}
	addRepoRoot := func(root string) {
		if root == "" {
			return
		}
		clean := filepath.Clean(root)
		if seenRoots[clean] {
			return
		}
		if isReadableRepoTodoRoot(clean) {
			seenRoots[clean] = true
			snap.repoStores = append(snap.repoStores, todopkg.RepoStore(clean))
		}
	}

	if snap.board != nil {
		for _, s := range snap.board.Stories {
			for _, r := range sprintDeclaredStoryRoots(s) {
				addRepoRoot(r)
			}
		}
	}

	if cwd, err := os.Getwd(); err == nil {
		if root, err := weaveRepoRoot(cwd); err == nil && root != "" {
			snap.cwdGitRoot = filepath.Clean(root)
			snap.cwdReadable = isReadableRepoTodoRoot(snap.cwdGitRoot)
			addRepoRoot(snap.cwdGitRoot)
			// Check immediate git submodules of an umbrella checkout when readable.
			if entries, rerr := os.ReadDir(snap.cwdGitRoot); rerr == nil {
				for _, e := range entries {
					if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
						continue
					}
					sub := filepath.Join(snap.cwdGitRoot, e.Name())
					if _, gerr := os.Stat(filepath.Join(sub, ".git")); gerr == nil {
						if !isReadableRepoTodoRoot(sub) {
							snap.cwdReadable = false
							continue
						}
						addRepoRoot(sub)
					}
				}
			}
		}
	}

	if todoRoot, err := todopkg.Root(); err == nil && todoRoot != "" {
		entries, rerr := os.ReadDir(todoRoot)
		if rerr != nil {
			if !os.IsNotExist(rerr) {
				snap.userReadable = false
			}
		} else {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				ownerDir := filepath.Join(todoRoot, e.Name())
				if _, oerr := os.ReadDir(ownerDir); oerr != nil && !os.IsNotExist(oerr) {
					snap.userReadable = false
					continue
				}
				snap.userStores = append(snap.userStores, &issue.Store{Root: todoRoot, Sub: e.Name()})
			}
		}
	}
	return snap, nil
}

func (snap *closedStorySnapshot) findStory(storyID string) *issue.Issue {
	for _, st := range snap.repoStores {
		if it, err := todopkg.ResolveRef(st, storyID); err == nil && it != nil {
			return it
		}
	}
	for _, st := range snap.userStores {
		if it, err := todopkg.ResolveRef(st, storyID); err == nil && it != nil {
			return it
		}
	}
	return nil
}

// classifyStoryClosure determines whether a story reference is provably closed
// or provably missing, refusing whenever a required store is unreadable or not
// checked out on this host.
func (snap *closedStorySnapshot) classifyStoryClosure(sprintSeq int64, storyID string, isStoryKind bool) (bool, string) {
	var s *weaveStory
	if sprintSeq > 0 && snap.board != nil {
		s = findWeaveStory(snap.board, sprintSeq)
	}
	if s != nil && !sprintColumnOpen(s.Column) {
		return true, fmt.Sprintf("sprint #%d is done", s.ID)
	}

	if it := snap.findStory(storyID); it != nil {
		if todopkg.IsClosed(it.Status) {
			return true, fmt.Sprintf("story %s is %s", shortSprintStoryID(it.ID), it.Status)
		}
		if sprintSeq > 0 && s != nil && !storyBelongsToSprint(it, s) {
			if isStoryKind {
				return true, fmt.Sprintf("story %s moved off sprint #%d", shortSprintStoryID(it.ID), sprintSeq)
			}
			// A lease claim explicitly naming `<sprint>/<story>` belongs to that sprint's assignment.
			return true, fmt.Sprintf("story %s moved off sprint #%d", shortSprintStoryID(it.ID), sprintSeq)
		}
		return false, "story is still " + it.Status
	}

	// Story was not found in any readable store. Distinguish a provably deleted
	// story from an unmounted/unreadable repository or foreign sprint number.
	if sprintSeq > 0 {
		if s == nil {
			return false, "sprint card not present on this host"
		}
		roots := sprintDeclaredStoryRoots(s)
		if len(roots) == 0 {
			return false, "open sprint has no declared story roots"
		}
		for _, r := range roots {
			if !isReadableRepoTodoRoot(r) {
				return false, "sprint story store unavailable or unreadable: " + r
			}
		}
		if !snap.userReadable {
			return false, "host todo store unreadable"
		}
		return true, fmt.Sprintf("story %s no longer exists in sprint #%d store(s)", shortSprintStoryID(storyID), sprintSeq)
	}

	// Bare story tag (`--story <id>` with no sprint prefix): without a sprint
	// card naming authoritative StoryRoots, absence from cwd does not prove the
	// story was deleted from whichever repository originally owned it.
	return false, "bare story has no authoritative declared store"
}

func claimStoryClosureTarget(c *coord.Claim) (sprintSeq int64, storyID string, isStoryKind bool, ok bool) {
	if c == nil {
		return 0, "", false, false
	}
	if c.Kind == storyKind {
		seq, id, parsed := parseStoryClaimTag(c.Resource)
		return seq, id, true, parsed
	}
	if strings.TrimSpace(c.Story) != "" {
		seq, id, parsed := parseStoryClaimTag(c.Story)
		return seq, id, false, parsed
	}
	return 0, "", false, false
}

func storyForWeaveItem(board *weaveQueue, queueDir string, it *weaveItem) (int64, string, bool) {
	if it == nil {
		return 0, "", false
	}
	if len(it.WorkerStories) > 0 && it.WorkerStories[0].ID != "" {
		return it.WorkerStories[0].Sprint, it.WorkerStories[0].ID, true
	}
	qBase := filepath.Base(queueDir)
	if board != nil {
		for _, s := range board.Stories {
			for _, r := range s.Runs {
				if r.ID != it.ID || (r.Queue != "" && r.Queue != qBase) {
					continue
				}
				if !r.Born.IsZero() && !r.Born.Equal(it.Created) {
					continue
				}
				if r.Story != "" {
					return s.ID, r.Story, true
				}
				if it.Register != "" {
					return s.ID, it.Register, true
				}
				if !sprintColumnOpen(s.Column) {
					return s.ID, "", true
				}
			}
		}
	}
	if it.Register != "" {
		return 0, it.Register, true
	}
	return 0, "", false
}

func candidateWeaveQueueDirs(snap *closedStorySnapshot) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		clean := filepath.Clean(d)
		if seen[clean] {
			return
		}
		if _, err := os.Stat(filepath.Join(clean, "queue.json")); err == nil {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}
	if snap.board != nil {
		for _, s := range snap.board.Stories {
			for _, r := range s.Runs {
				if d, err := weaveQueueDirForSprintRun(r); err == nil {
					add(d)
				}
			}
			for _, root := range sprintDeclaredStoryRoots(s) {
				if d, err := weaveQueueDir(root); err == nil {
					add(d)
				}
			}
		}
	}
	if snap.cwdGitRoot != "" {
		if d, err := weaveQueueDir(snap.cwdGitRoot); err == nil {
			add(d)
		}
	}
	sort.Strings(dirs)
	return dirs
}

func (weaveClosedStoryPruner) DryRunClosedStories(ctx context.Context, coordDir string, opt prune.Options) ([]prune.Item, error) {
	snap, err := buildClosedStorySnapshot()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var items []prune.Item

	// 1. Stale claims on closed or provably missing stories.
	claims, err := coord.List(coordDir)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.Mode == coord.ModeAttached {
			continue
		}
		if opt.Older > 0 && now.Sub(c.Heartbeat) < opt.Older {
			continue
		}
		seq, sid, isStoryKind, ok := claimStoryClosureTarget(c)
		if !ok {
			continue
		}
		closed, reason := snap.classifyStoryClosure(seq, sid, isStoryKind)
		if !closed {
			continue
		}
		holder := c.Holder.Name
		if holder == "" {
			holder = "unattributed"
		}
		items = append(items, prune.Item{
			Noun:   "claim",
			ID:     c.Key(),
			Reason: fmt.Sprintf("%s (%s hold by %s, epoch %d, rev %d)", reason, c.Mode, holder, c.Epoch, c.Rev),
		})
	}

	// 2. Leftover grade checkouts of closed stories or ended sprints.
	if gradeRoot, gerr := sprintGradeRootDir(); gerr == nil {
		if sprintEntries, rerr := os.ReadDir(gradeRoot); rerr == nil {
			for _, se := range sprintEntries {
				if !se.IsDir() {
					continue
				}
				seq, perr := strconv.ParseInt(se.Name(), 10, 64)
				if perr != nil || seq <= 0 {
					continue
				}
				var card *weaveStory
				if snap.board != nil {
					card = findWeaveStory(snap.board, seq)
				}
				if card == nil {
					continue
				}
				sprintDir := filepath.Join(gradeRoot, se.Name())
				hashEntries, herr := os.ReadDir(sprintDir)
				if herr != nil {
					continue
				}
				for _, he := range hashEntries {
					hashDir := filepath.Join(sprintDir, he.Name())
					hinfo, lerr := os.Lstat(hashDir)
					if lerr != nil || hinfo.Mode()&os.ModeSymlink != 0 || !hinfo.IsDir() {
						continue
					}
					reason := ""
					eligible := false
					if !sprintColumnOpen(card.Column) {
						eligible = true
						reason = fmt.Sprintf("sprint #%d is done", seq)
					} else {
						for _, run := range card.Runs {
							label := fmt.Sprintf("%s#%d", run.Repo, run.ID)
							if sprintLeaseTokenHash(label)[:16] != he.Name() {
								continue
							}
							sid := run.Story
							if sid == "" {
								if qDir, qerr := weaveQueueDirForSprintRun(run); qerr == nil {
									if q, qerr := loadWeaveQueue(qDir); qerr == nil {
										if it := findWeaveItem(q, run.ID); it != nil {
											_, sid, _ = storyForWeaveItem(snap.board, qDir, it)
										}
									}
								}
							}
							if sid != "" {
								eligible, reason = snap.classifyStoryClosure(seq, sid, true)
							}
							break
						}
					}
					if !eligible {
						continue
					}
					attempts, aerr := os.ReadDir(hashDir)
					if aerr != nil {
						continue
					}
					for _, ae := range attempts {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						target := filepath.Join(hashDir, ae.Name())
						ainfo, lerr := os.Lstat(target)
						if lerr != nil || ainfo.Mode()&os.ModeSymlink != 0 {
							continue
						}
						if opt.Older > 0 && now.Sub(ainfo.ModTime()) < opt.Older {
							continue
						}
						size, serr := prune.ApparentSize(target)
						if serr != nil {
							continue
						}
						items = append(items, prune.Item{
							Noun:   "grade",
							ID:     target,
							Size:   size,
							Reason: reason + "; leftover grade checkout",
						})
					}
				}
			}
		}
	}

	// 3. Leftover settled weave run copies of closed stories or ended sprints.
	for _, qDir := range candidateWeaveQueueDirs(snap) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q, lerr := loadWeaveQueue(qDir)
		if lerr != nil {
			continue
		}
		root, ok := weaveRepoRootForQueue(qDir)
		if !ok {
			continue
		}
		repo := filepath.Base(root)
		for _, it := range q.Items {
			if opt.Older > 0 && (it.Created.IsZero() || now.Sub(it.Created) < opt.Older) {
				continue
			}
			seq, sid, linked := storyForWeaveItem(snap.board, qDir, it)
			if !linked {
				continue
			}
			var eligible bool
			var reason string
			if sid == "" && seq > 0 && snap.board != nil {
				if card := findWeaveStory(snap.board, seq); card != nil && !sprintColumnOpen(card.Column) {
					eligible, reason = true, fmt.Sprintf("sprint #%d is done", seq)
				}
			} else if sid != "" {
				eligible, reason = snap.classifyStoryClosure(seq, sid, true)
			}
			if !eligible {
				continue
			}
			for _, a := range weavePlanOwnedRunWithOptions(qDir, repo, it, true) {
				if a.Err != "" {
					continue
				}
				items = append(items, prune.Item{
					Noun:   "weave",
					ID:     fmt.Sprintf("%s#%d:%s:%s", qDir, it.ID, a.Kind, a.Target),
					Size:   int64(a.ExpectedBytes),
					Reason: fmt.Sprintf("%s; settled run %s#%d %s", reason, repo, it.ID, a.Kind),
				})
			}
		}
	}

	return items, nil
}

func parseWeaveBackfillID(raw string) (queueDir string, runID int64, kind, target string, ok bool) {
	hashIdx := strings.LastIndexByte(raw, '#')
	if hashIdx <= 0 {
		return "", 0, "", "", false
	}
	// Because Target can contain '#', split from the first ':' after queueDir#runID:
	// queueDir has no '#' in conventional ~/.bashy/weave/<repo>-<hash>, but guard
	// by scanning '#' positions where the segment up to ':' parses as positive int64.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '#' {
			continue
		}
		rest := raw[i+1:]
		colon1 := strings.IndexByte(rest, ':')
		if colon1 <= 0 {
			continue
		}
		id, err := strconv.ParseInt(rest[:colon1], 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		afterID := rest[colon1+1:]
		colon2 := strings.IndexByte(afterID, ':')
		if colon2 <= 0 {
			continue
		}
		return raw[:i], id, afterID[:colon2], afterID[colon2+1:], true
	}
	return "", 0, "", "", false
}

type expectedClaimState struct {
	hasEpoch bool
	epoch    uint64
	hasRev   bool
	rev      uint64
}

func parseExpectedClaimState(reason string) expectedClaimState {
	var st expectedClaimState
	if idx := strings.LastIndex(reason, "epoch "); idx >= 0 {
		rest := reason[idx+len("epoch "):]
		end := strings.IndexAny(rest, ",)")
		if end > 0 {
			if ep, err := strconv.ParseUint(strings.TrimSpace(rest[:end]), 10, 64); err == nil {
				st.hasEpoch, st.epoch = true, ep
			}
		}
	}
	if idx := strings.LastIndex(reason, "rev "); idx >= 0 {
		rest := reason[idx+len("rev "):]
		end := strings.IndexAny(rest, ",)")
		if end > 0 {
			if rv, err := strconv.ParseUint(strings.TrimSpace(rest[:end]), 10, 64); err == nil {
				st.hasRev, st.rev = true, rv
			}
		}
	}
	return st
}

func (weaveClosedStoryPruner) ApplyClosedStories(ctx context.Context, coordDir string, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	if len(items) == 0 {
		return counts, nil
	}
	snap, err := buildClosedStorySnapshot()
	if err != nil {
		return counts, err
	}

	wantClaims := map[string]expectedClaimState{}
	wantGrades := map[string]bool{}
	type runTarget struct {
		queueDir string
		runID    int64
	}
	wantRuns := map[runTarget]map[string]bool{}

	for _, item := range items {
		switch item.Noun {
		case "claim":
			wantClaims[item.ID] = parseExpectedClaimState(item.Reason)
		case "grade":
			wantGrades[filepath.Clean(item.ID)] = true
		case "weave":
			qDir, runID, kind, target, ok := parseWeaveBackfillID(item.ID)
			if !ok {
				return counts, fmt.Errorf("claim prune --stories-closed: invalid weave item %q", item.ID)
			}
			rt := runTarget{queueDir: qDir, runID: runID}
			if wantRuns[rt] == nil {
				wantRuns[rt] = map[string]bool{}
			}
			wantRuns[rt][kind+":"+target] = true
		default:
			return counts, fmt.Errorf("claim prune --stories-closed: unknown item noun %q", item.Noun)
		}
	}

	actor := weaveConductorName("")
	verb := "claim prune --stories-closed"

	if len(wantClaims) > 0 {
		var freshSnap *closedStorySnapshot
		reclaimed, rerr := coord.ReclaimClaims(coordDir, func(c *coord.Claim) (string, bool) {
			expected, ok := wantClaims[c.Key()]
			if !ok {
				return "", false
			}
			if expected.hasEpoch && c.Epoch != expected.epoch {
				return "", false
			}
			if expected.hasRev && c.Rev != expected.rev {
				return "", false
			}
			if freshSnap == nil {
				var serr error
				freshSnap, serr = buildClosedStorySnapshot()
				if serr != nil {
					return "", false
				}
			}
			seq, sid, isStoryKind, parsed := claimStoryClosureTarget(c)
			if !parsed {
				return "", false
			}
			closed, reason := freshSnap.classifyStoryClosure(seq, sid, isStoryKind)
			return reason, closed
		})
		if rerr != nil {
			return counts, rerr
		}
		for _, rc := range reclaimed {
			counts.Removed++
			seq, sid, _, _ := claimStoryClosureTarget(rc.Claim)
			holder := rc.Claim.Holder.Name
			if holder == "" {
				holder = "unattributed"
			}
			reclaimRecordTimeline(seq, sid, actor, verb,
				fmt.Sprintf("released claim %s (holder %s): %s", rc.Key, holder, rc.Reason))
		}
	}

	if len(wantGrades) > 0 {
		gradeRoot, gerr := sprintGradeRootDir()
		if gerr == nil {
			freshItems, derr := (weaveClosedStoryPruner{}).DryRunClosedStories(ctx, coordDir, prune.Options{})
			if derr != nil {
				return counts, derr
			}
			stillEligible := map[string]string{}
			for _, fi := range freshItems {
				if fi.Noun == "grade" {
					stillEligible[filepath.Clean(fi.ID)] = fi.Reason
				}
			}
			for target := range wantGrades {
				reason, ok := stillEligible[target]
				if !ok {
					continue
				}
				if err := weaveContainedArtifact(gradeRoot, target); err != nil {
					continue
				}
				info, lerr := os.Lstat(target)
				if lerr != nil || info.Mode()&os.ModeSymlink != 0 {
					continue
				}
				size, _ := prune.ApparentSize(target)
				cleanupGradeCheckout(target)
				if _, statErr := os.Lstat(target); os.IsNotExist(statErr) {
					counts.Removed++
					counts.Bytes += size
					reclaimRecordTimeline(0, "", actor, verb, fmt.Sprintf("deleted grade checkout %s (%s)", target, reason))
				}
			}
		}
	}

	if len(wantRuns) > 0 {
		for rt, allowed := range wantRuns {
			if err := ctx.Err(); err != nil {
				return counts, err
			}
			q, lerr := loadWeaveQueue(rt.queueDir)
			if lerr != nil {
				continue
			}
			it := findWeaveItem(q, rt.runID)
			if it == nil {
				continue
			}
			seq, sid, linked := storyForWeaveItem(snap.board, rt.queueDir, it)
			if !linked {
				continue
			}
			eligible := false
			if sid == "" && seq > 0 && snap.board != nil {
				if card := findWeaveStory(snap.board, seq); card != nil && !sprintColumnOpen(card.Column) {
					eligible = true
				}
			} else if sid != "" {
				eligible, _ = snap.classifyStoryClosure(seq, sid, true)
			}
			if !eligible {
				continue
			}
			root, ok := weaveRepoRootForQueue(rt.queueDir)
			if !ok {
				continue
			}
			acts := weavePruneOwnedRunWithOptions(rt.queueDir, rt.runID, filepath.Base(root), weavePruneOptions{EndRetain: true}, false)
			for _, a := range acts {
				if !a.Done || a.Err != "" || a.Kind == "lock" {
					continue
				}
				if allowed[a.Kind+":"+a.Target] {
					counts.Removed++
					counts.Bytes += int64(a.ActualBytes)
				}
				reclaimRecordTimeline(seq, sid, actor, verb, fmt.Sprintf("deleted %s %s", a.Kind, a.Target))
			}
		}
	}

	return counts, nil
}
