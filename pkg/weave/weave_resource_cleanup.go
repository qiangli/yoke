package weave

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
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

// Run dispositions. A disposition is the durable, one-word answer to "what
// happened to this run's work" — the only thing a later reader needs once the
// ephemeral artifacts are gone. It is recorded by the verb that decided it
// (pull → merged, abandon → superseded|rejected, an empty terminal run →
// empty) and derived once at cleanup for rows that predate the field.
const (
	weaveDispositionMerged     = "merged"
	weaveDispositionSuperseded = "superseded"
	weaveDispositionRejected   = "rejected"
	weaveDispositionEmpty      = "empty"
)

func weaveValidDisposition(d string) bool {
	switch d {
	case weaveDispositionMerged, weaveDispositionSuperseded, weaveDispositionRejected, weaveDispositionEmpty:
		return true
	}
	return false
}

// weaveItemSettled is THE proof that tearing down a run's workspace loses
// nothing: every commit in the workspace is reachable from base or from the
// run's salvage ref, and the tree is clean. It returns the disposition the row
// should carry. A workspace that is already gone cannot lose anything on disk
// (its branch, if fetched, is retired by its own proof); its disposition is
// what the row recorded, else merged/empty from the recorded measurement.
func weaveItemSettled(root, base string, it *weaveItem) (string, bool) {
	if it == nil {
		return "", false
	}
	if weaveItemMerged(root, base, it) {
		return weaveDispositionMerged, true
	}
	if it.Workspace == "" {
		if weaveValidDisposition(it.Disposition) {
			return it.Disposition, true
		}
		if it.CommitsAhead > 0 && it.Head != "" && base != "" &&
			exec.Command(gitBin(), "-C", root, "merge-base", "--is-ancestor", it.Head, base).Run() == nil {
			return weaveDispositionMerged, true
		}
		return weaveDispositionEmpty, it.CommitsAhead == 0
	}
	st, err := os.Stat(it.Workspace)
	if err != nil {
		if os.IsNotExist(err) {
			cp := *it
			cp.Workspace = ""
			return weaveItemSettled(root, base, &cp)
		}
		return "", false
	}
	if !st.IsDir() {
		return "", false
	}
	// Live measurement, never the recorded one (see weaveItemMerged).
	ahead, head := weaveUnmergedAhead(root, base, it)
	if ahead == 0 {
		if it.Disposition != "" && it.Disposition != weaveDispositionMerged {
			return it.Disposition, true
		}
		return weaveDispositionEmpty, true
	}
	if it.SalvageRef == "" || head == "" {
		return "", false
	}
	if exec.Command(gitBin(), "-C", root, "merge-base", "--is-ancestor", head, it.SalvageRef).Run() != nil {
		return "", false
	}
	if it.Disposition == weaveDispositionSuperseded {
		return weaveDispositionSuperseded, true
	}
	return weaveDispositionRejected, true
}

// weaveRunArtifactKinds is the ordered list of everything a run owns on disk.
// Order matters: the lock goes last, because this function holds it.
var weaveRunArtifactKinds = []string{"workspace", "socket", "log", "cache", "agent-data", "lock"}

func weaveRunArtifactPath(dir string, it *weaveItem, kind string) string {
	switch kind {
	case "workspace":
		return it.Workspace
	case "socket":
		return it.CtlSock
	case "log":
		return it.LogPath
	case "cache":
		return weaveManagedGOCachePath(nil, dir, it.ID)
	case "agent-data":
		return agentlaunch.YcodeDataDir(nil, agentlaunch.Launch{ToolName: agentlaunch.YcodeToolName}, dir, strconv.FormatInt(it.ID, 10))
	case "lock":
		return weaveRunLifecycleLockPath(dir, it.ID)
	}
	return ""
}

// weavePruneOwnedRun is the ONE guarded teardown. pull, salvage (via pull),
// abandon, prune and sprint end all reach it; none of them removes a run
// artifact any other way. It refuses rather than guesses: active lifecycle,
// changed birth, unproven settlement, dirty tree, unconventional path, symlink
// traversal, and a run that changed between the check and the rename all leave
// the artifact alone and say why. On full success it compacts the row (D4):
// paths are cleared, the disposition stays.
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
	base := weaveBaseBranch(root)
	disposition, settled := weaveItemSettled(root, base, it)
	if !settled {
		return nil
	}
	var acts []sprintPruneAction
	failed := false
	for _, kind := range weaveRunArtifactKinds {
		path := weaveRunArtifactPath(dir, it, kind)
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		}
		a := sprintPruneAction{Kind: kind, Repo: repo, Target: path, ByteKind: "apparent_regular_file_bytes"}
		if kind == "lock" {
			if failed {
				// Something of the run is still on disk; the lock stays with it.
				continue
			}
			// Held by this function; TryAcquire-only callers mean nobody sleeps
			// on the old inode, so unlinking while held cannot mint two holders.
			if err := os.Remove(path); err != nil {
				a.Err = err.Error()
				failed = true
			} else {
				a.Done = true
				a.BytesComplete = true
			}
			// Acquiring created this file; report it only when the pass
			// reclaimed something else, so an idle pass stays silent.
			if a.Err != "" || len(acts) > 0 {
				acts = append(acts, a)
			}
			continue
		}
		if err := weaveConventionalArtifact(dir, id, kind, path); err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			failed = true
			continue
		}
		if kind == "socket" {
			// A control socket may live under os.TempDir() when the queue path
			// is too long for a unix socket (weaveCtlSockPath); the conventional
			// check above already proved it is THIS run's exact path, and a
			// socket is one inode — remove it, never a tree under it.
			st, err := os.Lstat(path)
			if err == nil && st.Mode()&(os.ModeSocket|os.ModeSymlink) == 0 && !st.Mode().IsRegular() {
				err = errors.New("socket path is not a socket or file")
			}
			if err == nil {
				err = os.Remove(path)
			}
			if err != nil {
				a.Err = err.Error()
				failed = true
			} else {
				a.Done = true
				a.BytesComplete = true
			}
			acts = append(acts, a)
			continue
		}
		if err := weaveContainedArtifact(dir, path); err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			failed = true
			continue
		}
		if kind == "workspace" {
			// Include untracked/ignored files: cleanup must not discard private work.
			out, err := exec.Command(gitBin(), "-C", path, "status", "--porcelain", "--untracked-files=all", "--ignored").Output()
			if err != nil || len(out) > 0 {
				a.Err = "workspace dirty, untracked, ignored, or unreadable; left alone"
				acts = append(acts, a)
				failed = true
				continue
			}
		}
		expected, err := weaveArtifactBytes(path)
		a.ExpectedBytes = expected
		if err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			failed = true
			continue
		}
		// Claim by atomic rename under the fresh eligibility check. Long deletion
		// stays outside queue.lock; starts are excluded by the lifecycle lock.
		quarantine := path + fmt.Sprintf(".reclaim-%d", time.Now().UnixNano())
		err = withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
			current, err := weaveCleanupEligible(fresh, id, path)
			if err != nil {
				return err
			}
			if current.Workspace != it.Workspace || current.Head != it.Head || current.FinishedAt != it.FinishedAt || !current.Created.Equal(it.Created) || current.LogPath != it.LogPath || current.CtlSock != it.CtlSock {
				return errors.New("run changed during cleanup")
			}
			if _, ok := weaveItemSettled(root, base, current); !ok {
				return errors.New("settlement proof changed during cleanup")
			}
			return os.Rename(path, quarantine)
		})
		if err != nil {
			a.Err = err.Error()
			acts = append(acts, a)
			failed = true
			continue
		}
		// Recheck after claiming so an edit during the scan cannot be discarded.
		if kind == "workspace" {
			if e := weaveVerifyReclaimWorkspace(root, base, it, quarantine); e != nil {
				_ = os.Rename(quarantine, path)
				a.Err = e.Error()
				acts = append(acts, a)
				failed = true
				continue
			}
		}
		actual, e := weaveArtifactBytes(quarantine)
		if e != nil {
			_ = os.Rename(quarantine, path)
			a.Err = e.Error()
			acts = append(acts, a)
			failed = true
			continue
		}
		if e = os.RemoveAll(quarantine); e != nil {
			a.Err = e.Error()
			failed = true
		} else {
			a.Done = true
			a.ActualBytes = actual
			a.BytesComplete = true
		}
		acts = append(acts, a)
	}
	// Compact the row: the disposition is the durable outcome; the paths
	// pointed at bytes that no longer exist. A partial failure keeps every
	// path so the next pass (and the operator) can still find the leftover.
	_ = withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
		cur := findWeaveItem(fresh, id)
		if cur == nil {
			return nil
		}
		if cur.Disposition == "" {
			cur.Disposition = disposition
		}
		if !failed {
			cur.Workspace, cur.LogPath, cur.CtlSock = "", "", ""
		}
		return nil
	})
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
		if !ok {
			continue
		}
		if _, settled := weaveItemSettled(root, weaveBaseBranch(root), it); !settled {
			continue
		}
		for _, kind := range weaveRunArtifactKinds {
			path := weaveRunArtifactPath(dir, it, kind)
			if path == "" {
				continue
			}
			if _, e := os.Lstat(path); e != nil {
				continue
			}
			a := sprintPruneAction{Kind: kind, Repo: run.Repo, Target: path, ByteKind: "apparent_regular_file_bytes"}
			if e := weaveContainedArtifact(dir, path); e != nil {
				a.Err = e.Error()
			} else {
				a.ExpectedBytes, e = weaveArtifactBytes(path)
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
	case "cache":
		expected = weaveManagedGOCachePath(nil, dir, id)
	case "agent-data":
		expected = agentlaunch.YcodeDataDir(nil, agentlaunch.Launch{ToolName: agentlaunch.YcodeToolName}, dir, strconv.FormatInt(id, 10))
	}
	if expected == "" || filepath.Clean(path) != filepath.Clean(expected) {
		return errors.New("artifact is not this run's conventional owned path")
	}
	return nil
}
func weaveVerifyReclaimWorkspace(root, base string, it *weaveItem, path string) error {
	cp := *it
	cp.Workspace = path
	if _, ok := weaveItemSettled(root, base, &cp); !ok {
		return errors.New("claimed workspace gained unintegrated work; left alone")
	}
	out, e := exec.Command(gitBin(), "-C", path, "status", "--porcelain", "--untracked-files=all", "--ignored").Output()
	if e != nil || len(out) > 0 {
		return errors.New("claimed workspace gained uncommitted work; left alone")
	}
	return nil
}
