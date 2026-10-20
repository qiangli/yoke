package weave

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/gomod"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func sprintRunRoot(run sprintRun) (string, bool) {
	dir, err := weaveQueueDirForSprintRun(run)
	if err != nil {
		return "", false
	}
	return weaveRepoRootForQueue(dir)
}

// Attribute individual paths, not the entire umbrella. Story membership wins
// over repo ownership; otherwise the most specific linked repo owns its files
// and its gitlink in the parent checkout. Equal owners still include our work
// unless the run records show ours delivered and theirs producing.
func sprintDirtyOwnership(s *weaveStory, board []*weaveStory, root string, repoPath func(sprintRun) (string, bool)) (int, []string, error) {
	root = hygieneRootKey(root)
	out, err := exec.Command(gitBin(), "-C", root, "status", "--porcelain", "-z", "--untracked-files=all").Output()
	if err != nil {
		return 0, nil, err
	}
	candidates, roots := sprintOwnedRoots(s, board, repoPath)
	dirty := 0
	var warnings []string
	// Read the delivery history only once something is attributed to us by
	// mere containment — a clean checkout, or one dirtied only outside our
	// links, never pays for it.
	var footprint *sprintFootprint
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		paths := []string{entry[3:]}
		if strings.ContainsAny(entry[:2], "RC") && i+1 < len(entries) {
			i++
			paths = append(paths, entries[i])
		}
		// Only an untracked path lets the footprint's directory claims speak
		// for it; anything git already tracks is attributed path-level, from
		// the history it has of its own.
		untracked := entry[:2] == "??"
		owners := map[int64]bool{}
		// hard records an attribution the sprint's commit footprint cannot
		// narrow: its own story file, or a linked checkout's own state.
		hard := false
		for _, path := range paths {
			abs := hygieneRootKey(filepath.Join(root, path))
			// Read deleted/renamed stories from HEAD so removing the file does not
			// erase responsibility for the uncommitted deletion.
			storyPath := strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(todopkg.RepoSub)+"/")
			if storyPath {
				data, readErr := os.ReadFile(filepath.Join(root, path))
				if readErr != nil {
					data, _ = exec.Command(gitBin(), "-C", root, "show", "HEAD:"+filepath.ToSlash(path)).Output()
				}
				if it, parseErr := issue.Parse(data); parseErr == nil {
					for _, owner := range candidates {
						if storyBelongsToSprint(it, owner) {
							owners[owner.ID] = true
							hard = hard || owner.ID == s.ID
						}
					}
					continue
				}
			}
			rooted := sprintRootOwners(s, roots, abs, true)
			for id := range rooted {
				owners[id] = true
			}
			hard = hard || (rooted[s.ID] && sprintOwnsRootExactly(roots, abs, s.ID))
		}
		if owners[s.ID] && !hard {
			if footprint == nil {
				footprint = sprintCommitFootprint(root)
			}
			if footprint.records(s.ID) && !sprintTouchedAny(footprint, s.ID, paths, untracked) {
				delete(owners, s.ID)
				for _, path := range paths {
					for id := range footprint.owners(filepath.ToSlash(path), untracked) {
						owners[id] = true
					}
				}
			}
		}
		if owners[s.ID] {
			dirty++
			continue
		}
		owner := sprintOwnerNames(owners)
		warnings = append(warnings, fmt.Sprintf("WARNING: %s: uncommitted %s (%s)", root, strings.Join(paths, " <- "), owner))
	}
	return dirty, warnings, nil
}

type sprintOwnedRoot struct {
	path string
	id   int64
	run  sprintRun
}

// sprintOwnedRoots lists the checkouts linked by s and every other active sprint.
func sprintOwnedRoots(s *weaveStory, board []*weaveStory, repoPath func(sprintRun) (string, bool)) ([]*weaveStory, []sprintOwnedRoot) {
	candidates := []*weaveStory{s}
	for _, other := range board {
		if other != nil && other.ID != s.ID && other.currentBox() != nil {
			candidates = append(candidates, other)
		}
	}
	var roots []sprintOwnedRoot
	for _, owner := range candidates {
		for _, run := range owner.Runs {
			if path, ok := repoPath(run); ok {
				roots = append(roots, sprintOwnedRoot{hygieneRootKey(path), owner.ID, run})
			}
		}
	}
	return candidates, roots
}

// sprintRootOwners names the sprints whose most specific linked checkout holds
// abs. A checkout linked by s and another active sprint is ambiguous and stays
// s's unless yield is set and the run records settle it (sprintYieldsRoot).
func sprintRootOwners(s *weaveStory, roots []sprintOwnedRoot, abs string, yield bool) map[int64]bool {
	longest, root := -1, ""
	owners := map[int64]bool{}
	for _, owned := range roots {
		if abs != owned.path && !strings.HasPrefix(abs, owned.path+string(filepath.Separator)) {
			continue
		}
		if len(owned.path) > longest {
			longest, root = len(owned.path), owned.path
			owners = map[int64]bool{}
		}
		if len(owned.path) == longest {
			owners[owned.id] = true
		}
	}
	if yield && owners[s.ID] && len(owners) > 1 && sprintYieldsRoot(s.ID, roots, root) {
		delete(owners, s.ID)
	}
	return owners
}

// sprintYieldsRoot attributes a shared checkout's dirt to the other active
// sprints only on run evidence: every run s linked there is finished with
// nothing unmerged and nothing awaiting a decision, while another sprint still
// has a run producing there. A run record that cannot be read keeps it ours.
func sprintYieldsRoot(id int64, roots []sprintOwnedRoot, root string) bool {
	producing := false
	for _, owned := range roots {
		if owned.path != root {
			continue
		}
		it := sprintLinkedItem(owned.run)
		if owned.id == id {
			if it == nil || !isPrunableState(it.State) || it.UnmergedCommits > 0 || it.NeedsSteward || it.CleanupError != "" {
				return false
			}
		} else if it != nil && !isPrunableState(it.State) {
			producing = true
		}
	}
	return producing
}

// sprintStalePinOwnership keeps the stale pins s answers for. A pin goes stale
// when its sibling's HEAD moves, so it belongs to whoever produces in the
// sibling: a pin on a sibling only other active sprints link is warned, as is
// one in a dep this sprint never changed. A sibling s links, or delivered a
// commit in, still blocks — the pin is the only sibling source CI reads, so
// shipping a change there and leaving the pin behind is this sprint's chore.
//
// Scoping a pin to the deps s changed needs s to have delivered something in
// the module that holds the pin; with no commits of its own there, nothing is
// scoped and an unlinked sibling's drift stays s's to answer, as before.
func sprintStalePinOwnership(s *weaveStory, board []*weaveStory, path string, pins []string, repoPath func(sprintRun) (string, bool)) ([]string, []string) {
	ws, err := gomod.Load(path)
	if err != nil || ws == nil {
		return pins, nil
	}
	_, roots := sprintOwnedRoots(s, board, repoPath)
	scoped := len(pins) > 0 && sprintCommitFootprint(path).records(s.ID)
	var own, warnings []string
	for _, name := range pins {
		sibling := hygieneRootKey(filepath.Join(ws.Root, filepath.FromSlash(name)))
		owners := sprintRootOwners(s, roots, sibling, false)
		if owners[s.ID] {
			own = append(own, name)
			continue
		}
		// Nobody links it: ask the sibling's own history whether this sprint
		// delivered there, and name whoever moved it past the pin when it
		// did not.
		if len(owners) == 0 {
			if !scoped || sprintCommitFootprint(sibling).records(s.ID) {
				own = append(own, name)
				continue
			}
			owners = sprintStalePinMovers(sibling)
		}
		warnings = append(warnings, fmt.Sprintf("WARNING: %s: stale pin %s (%s)", path, name, sprintOwnerNames(owners)))
	}
	return own, warnings
}

func sprintOwnerNames(owners map[int64]bool) string {
	var names []string
	for id := range owners {
		names = append(names, fmt.Sprintf("sprint #%d", id))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "unattributed"
	}
	return strings.Join(names, ", ")
}
