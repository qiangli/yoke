package weave

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Apparent bytes removed are not a claim about physical free space: shared
// extents, sparse files and filesystem metadata make those different metrics.
func weaveArtifactBytes(path string) (uint64, error) {
	var bytes uint64
	entries := 0
	deadline := time.Now().Add(2 * time.Second)
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 100000 || time.Now().After(deadline) {
			return errors.New("artifact scan bound exceeded")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			bytes += uint64(info.Size())
		}
		return nil
	})
	return bytes, err
}
func weaveContainedArtifact(dir, path string) error {
	base, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("artifact is outside its queue")
	}
	// Refuse symlink traversal below the authorized queue. Platform aliases
	// such as macOS /var -> /private/var are outside this ownership boundary.
	for p := target; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("artifact path traverses a symlink")
		}
		if p == base {
			break
		}
	}
	return nil
}
func weaveCleanupEligible(q *weaveQueue, id int64, path string) (*weaveItem, error) {
	it := findWeaveItem(q, id)
	if it == nil || !weavePrunableForSweep(it.State, false) || it.UnmergedCommits > 0 {
		return nil, errors.New("run is no longer terminal and integrated")
	}
	if it.ResourceReservationID != "" && !it.ResourceTerminated {
		return nil, errors.New("run child termination remains unverified")
	}
	if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
		return nil, errors.New("run wrapper may still be active")
	}
	for _, other := range q.Items {
		if other.ID == id {
			continue
		}
		if path != "" && (other.Workspace == path || other.LogPath == path || other.CtlSock == path) {
			return nil, fmt.Errorf("artifact also belongs to run #%d", other.ID)
		}
	}
	cp := *it
	return &cp, nil
}
func weavePruneOwnedRun(dir string, id int64, repo string, expectedBirth ...time.Time) []sprintPruneAction {
	lock, err := weaveRunLifecycleLock(dir, id)
	if err != nil {
		return []sprintPruneAction{{Kind: "run", Repo: repo, Target: fmt.Sprint(id), Err: "active lifecycle; left alone"}}
	}
	defer lock.Release()
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil
	}
	it, err := weaveCleanupEligible(q, id, "")
	if err != nil {
		return nil
	}
	if len(expectedBirth) > 0 && (expectedBirth[0].IsZero() || !expectedBirth[0].Equal(it.Created)) {
		return []sprintPruneAction{{Kind: "run", Repo: repo, Target: fmt.Sprint(id), Err: "sprint run birth unknown or changed; left alone"}}
	}
	root, ok := weaveRepoRootForQueue(dir)
	if !ok {
		return nil
	}
	if !weaveItemMerged(root, weaveBaseBranch(root), it) {
		return nil
	}
	var acts []sprintPruneAction
	for _, artifact := range []struct{ kind, path string }{{"workspace", it.Workspace}, {"socket", it.CtlSock}, {"log", it.LogPath}} {
		if artifact.path == "" {
			continue
		}
		if _, err := os.Lstat(artifact.path); os.IsNotExist(err) {
			continue
		}
		a := sprintPruneAction{Kind: artifact.kind, Repo: repo, Target: artifact.path, ByteKind: "apparent_regular_file_bytes"}
		if err := weaveConventionalArtifact(dir, id, artifact.kind, artifact.path); err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			continue
		}
		if err := weaveContainedArtifact(dir, artifact.path); err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			continue
		}
		if artifact.kind == "workspace" {
			rel, _ := filepath.Rel(filepath.Join(dir, "workspaces"), artifact.path)
			if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				a.Err = "workspace outside queue workspaces"
				acts = append(acts, a)
				continue
			}
			// Include untracked/ignored files: cleanup must not discard private work.
			out, err := exec.Command("git", "-C", artifact.path, "status", "--porcelain", "--untracked-files=all", "--ignored").Output()
			if err != nil || len(out) > 0 {
				a.Err = "workspace dirty, untracked, ignored, or unreadable; left alone"
				acts = append(acts, a)
				continue
			}
		}
		expected, err := weaveArtifactBytes(artifact.path)
		a.ExpectedBytes = expected
		if err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			continue
		}
		// Claim by atomic rename under the fresh eligibility check. Long deletion
		// stays outside queue.lock; starts are excluded by the lifecycle lock.
		quarantine := artifact.path + fmt.Sprintf(".reclaim-%d", time.Now().UnixNano())
		err = withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
			current, err := weaveCleanupEligible(fresh, id, artifact.path)
			if err != nil {
				return err
			}
			if current.Workspace != it.Workspace || current.Head != it.Head || current.FinishedAt != it.FinishedAt || !current.Created.Equal(it.Created) || current.LogPath != it.LogPath || current.CtlSock != it.CtlSock {
				return errors.New("run changed during cleanup")
			}
			if !weaveItemMerged(root, weaveBaseBranch(root), current) {
				return errors.New("integration proof changed during cleanup")
			}
			return os.Rename(artifact.path, quarantine)
		})
		if err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			continue
		}
		// Recheck after claiming so an edit during the scan cannot be discarded.
		if artifact.kind == "workspace" {
			out, e := exec.Command("git", "-C", quarantine, "status", "--porcelain", "--untracked-files=all", "--ignored").Output()
			if e != nil || len(out) > 0 {
				_ = os.Rename(quarantine, artifact.path)
				a.Err = "workspace changed while claiming; left alone"
				acts = append(acts, a)
				continue
			}
		}
		actual, e := weaveArtifactBytes(quarantine)
		if e != nil {
			_ = os.Rename(quarantine, artifact.path)
			a.Err = e.Error()
			acts = append(acts, a)
			continue
		}
		if artifact.kind == "workspace" {
			if e = weaveVerifyReclaimWorkspace(root, weaveBaseBranch(root), it, quarantine); e != nil {
				_ = os.Rename(quarantine, artifact.path)
				a.Err = e.Error()
				acts = append(acts, a)
				continue
			}
		}
		if e = os.RemoveAll(quarantine); e != nil {
			a.Err = e.Error()
		} else {
			a.Done = true
			a.ActualBytes = actual
			a.BytesComplete = true
		}
		acts = append(acts, a)
	}
	return acts
}

// A read-only estimate for review before --apply. No lock files, claims, or
// lifecycle writes occur here; apply independently repeats every eligibility
// check, and reports actual apparent bytes only after successful removal.
func sprintPlanRunArtifacts(s *weaveStory) []sprintPruneAction {
	var actions []sprintPruneAction
	if s == nil {
		return actions
	}
	for _, run := range s.Runs {
		dir, e := weaveQueueDirForSprintRun(run)
		if e != nil {
			continue
		}
		q, e := loadWeaveQueue(dir)
		if e != nil {
			continue
		}
		it, e := weaveCleanupEligible(q, run.ID, "")
		if e != nil {
			continue
		}
		if run.Born.IsZero() || !run.Born.Equal(it.Created) {
			continue
		}
		root, ok := weaveRepoRootForQueue(dir)
		if !ok || !weaveItemMerged(root, weaveBaseBranch(root), it) {
			continue
		}
		for _, artifact := range []struct{ kind, path string }{{"workspace", it.Workspace}, {"socket", it.CtlSock}, {"log", it.LogPath}} {
			if artifact.path == "" {
				continue
			}
			if _, e := os.Lstat(artifact.path); e != nil {
				continue
			}
			a := sprintPruneAction{Kind: artifact.kind, Repo: run.Repo, Target: artifact.path, ByteKind: "apparent_regular_file_bytes"}
			if e := weaveContainedArtifact(dir, artifact.path); e != nil {
				a.Err = e.Error()
			} else {
				a.ExpectedBytes, e = weaveArtifactBytes(artifact.path)
				a.BytesComplete = e == nil
				if e != nil {
					a.Err = e.Error()
				}
			}
			actions = append(actions, a)
		}
	}
	return actions
}

func weaveConventionalArtifact(dir string, id int64, kind, path string) error {
	expected := ""
	switch kind {
	case "workspace":
		expected = filepath.Join(dir, "workspaces", fmt.Sprintf("issue-%d", id))
	case "log":
		expected = filepath.Join(dir, "logs", fmt.Sprintf("issue-%d.log", id))
	case "socket":
		expected = weaveCtlSockPath(dir, id)
	}
	if expected == "" || filepath.Clean(path) != filepath.Clean(expected) {
		return errors.New("artifact is not this run's conventional owned path")
	}
	return nil
}
func weaveVerifyReclaimWorkspace(root, base string, it *weaveItem, path string) error {
	cp := *it
	cp.Workspace = path
	if !weaveItemMerged(root, base, &cp) {
		return errors.New("claimed workspace gained unintegrated work; left alone")
	}
	out, e := exec.Command("git", "-C", path, "status", "--porcelain", "--untracked-files=all", "--ignored").Output()
	if e != nil || len(out) > 0 {
		return errors.New("claimed workspace gained uncommitted work; left alone")
	}
	return nil
}
