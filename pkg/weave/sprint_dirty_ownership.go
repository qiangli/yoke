package weave

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
// and its gitlink in the parent checkout. Equal owners still include our work.
func sprintDirtyOwnership(s *weaveStory, board []*weaveStory, root string, repoPath func(sprintRun) (string, bool)) (int, []string, error) {
	root = hygieneRootKey(root)
	out, err := exec.Command(gitBin(), "-C", root, "status", "--porcelain", "-z", "--untracked-files=all").Output()
	if err != nil {
		return 0, nil, err
	}
	candidates := []*weaveStory{s}
	for _, other := range board {
		if other != nil && other.ID != s.ID && other.currentBox() != nil {
			candidates = append(candidates, other)
		}
	}
	type ownedRoot struct {
		path string
		id   int64
	}
	var roots []ownedRoot
	for _, owner := range candidates {
		for _, run := range owner.Runs {
			if path, ok := repoPath(run); ok {
				roots = append(roots, ownedRoot{hygieneRootKey(path), owner.ID})
			}
		}
	}
	dirty := 0
	var warnings []string
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
		owners := map[int64]bool{}
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
						}
					}
					continue
				}
			}
			longest := -1
			pathOwners := map[int64]bool{}
			for _, owned := range roots {
				if abs != owned.path && !strings.HasPrefix(abs, owned.path+string(filepath.Separator)) {
					continue
				}
				if len(owned.path) > longest {
					longest = len(owned.path)
					pathOwners = map[int64]bool{}
				}
				if len(owned.path) == longest {
					pathOwners[owned.id] = true
				}
			}
			for id := range pathOwners {
				owners[id] = true
			}
		}
		if owners[s.ID] {
			dirty++
			continue
		}
		var names []string
		for id := range owners {
			names = append(names, fmt.Sprintf("sprint #%d", id))
		}
		sort.Strings(names)
		owner := "unattributed"
		if len(names) > 0 {
			owner = strings.Join(names, ", ")
		}
		warnings = append(warnings, fmt.Sprintf("WARNING: %s: uncommitted %s (%s)", root, strings.Join(paths, " <- "), owner))
	}
	return dirty, warnings, nil
}
