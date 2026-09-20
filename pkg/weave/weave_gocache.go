package weave

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/gate"
)

const (
	weaveManagedGOCacheEnv = "GOCACHE"
	weaveManagedGOCacheDir = "go-build-cache"
)

var weaveManagedGOCacheReplacer = strings.NewReplacer(
	string(filepath.Separator), "_",
	string(os.PathListSeparator), "_",
	" ", "_",
)

func weaveManagedGOCachePath(parentEnv []string, queueDir string, issueID int64) string {
	if issueID <= 0 || strings.TrimSpace(queueDir) == "" || envHasName(parentEnv, weaveManagedGOCacheEnv) {
		return ""
	}
	return filepath.Join(queueDir, "agent-data", weaveManagedGOCacheDir, "run-"+weaveManagedGOCacheReplacer.Replace(strconv.FormatInt(issueID, 10)))
}

func weaveApplyManagedGOCache(env, parentEnv []string, queueDir string, it *weaveItem) []string {
	if it == nil {
		return env
	}
	dir := weaveManagedGOCachePath(parentEnv, queueDir, it.ID)
	if dir == "" || envHasName(env, weaveManagedGOCacheEnv) {
		return env
	}
	return append(env, weaveManagedGOCacheEnv+"="+dir)
}

func weaveVerifyEnv(parentEnv []string, workspace, queueDir string, it *weaveItem) []string {
	env := make([]string, 0, len(parentEnv)+2)
	for _, kv := range parentEnv {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PWD="+workspace)
	// A verify command's exit code is a VERDICT. It must not inherit bashy's own
	// controls, or a gate would decide differently depending on the shell that
	// launched the run. gate.Env's strict allowlist is not usable here: verify
	// runs an arbitrary operator-supplied build or test command, which needs the
	// wider toolchain environment no fixed list can enumerate. ScrubControls is
	// the narrower guarantee that fits — see its doc comment for the difference.
	env = gate.ScrubControls(env)
	return weaveApplyManagedGOCache(env, parentEnv, queueDir, it)
}

func weaveManagedGOCacheRoot(queueDir string) string {
	return filepath.Join(queueDir, "agent-data", weaveManagedGOCacheDir)
}

func weaveManagedGOCacheIssue(absQueueDir, cachePath string) (int64, bool) {
	root := weaveManagedGOCacheRoot(absQueueDir)
	rel, err := filepath.Rel(root, cachePath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." || strings.ContainsRune(rel, filepath.Separator) {
		return 0, false
	}
	base := filepath.Base(cachePath)
	if !strings.HasPrefix(base, "run-") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(base, "run-"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func safeRemoveManagedGOCache(queueDir, cachePath string) error {
	if cachePath == "" {
		return nil
	}
	absQueue, err := filepath.Abs(queueDir)
	if err != nil {
		return err
	}
	absCache, err := filepath.Abs(cachePath)
	if err != nil {
		return err
	}
	root := weaveManagedGOCacheRoot(absQueue)
	rel, err := filepath.Rel(root, absCache)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." || strings.ContainsRune(rel, filepath.Separator) {
		return fmt.Errorf("managed GOCACHE path %q is not contained in %q", absCache, root)
	}
	if q, err := loadWeaveQueue(absQueue); err == nil {
		for _, it := range q.Items {
			if it == nil || it.ID <= 0 {
				continue
			}
			if !(it.State == "working" || it.State == "finalizing") {
				continue
			}
			if it.WrapperPid == 0 && it.FinalizerPID == 0 {
				continue
			}
			live := (it.WrapperPid > 0 && pidAlive(it.WrapperPid)) || (it.FinalizerPID > 0 && pidAlive(it.FinalizerPID))
			if !live {
				continue
			}
			liveCache := weaveManagedGOCachePath(nil, absQueue, it.ID)
			if liveCache == "" {
				continue
			}
			liveCache, err = filepath.Abs(liveCache)
			if err != nil {
				return fmt.Errorf("managed GOCACHE live-run check failed: %w", err)
			}
			if liveCache == absCache {
				return fmt.Errorf("refusing to remove managed GOCACHE %q: run #%d is active", absCache, it.ID)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("managed GOCACHE live-run check failed: %w", err)
	}
	st, err := os.Stat(absCache)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("managed GOCACHE path %q is not a directory", absCache)
	}
	return os.RemoveAll(absCache)
}

func weaveCleanupManagedGOCache(queueDir string, it *weaveItem) error {
	if it == nil || queueDir == "" {
		return nil
	}
	cache := weaveManagedGOCachePath(nil, queueDir, it.ID)
	if cache == "" {
		return nil
	}
	return safeRemoveManagedGOCache(queueDir, cache)
}

type weaveManagedGOCacheSweep struct {
	Issue  int64
	Path   string
	Orphan bool
}

func weaveManagedGOCacheSweepTargets(queueDir string, q *weaveQueue, stale bool) ([]weaveManagedGOCacheSweep, error) {
	root := weaveManagedGOCacheRoot(queueDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	absQueue, err := filepath.Abs(queueDir)
	if err != nil {
		return nil, err
	}
	byIssue := make(map[int64]*weaveItem, len(q.Items))
	for _, it := range q.Items {
		if it != nil {
			byIssue[it.ID] = it
		}
	}
	seen := map[string]bool{}
	var out []weaveManagedGOCacheSweep
	for _, it := range q.Items {
		if it == nil || !weavePrunableForSweep(it.State, stale) {
			continue
		}
		cache := weaveManagedGOCachePath(nil, absQueue, it.ID)
		if cache == "" {
			continue
		}
		if _, err := os.Stat(cache); err == nil && !seen[cache] {
			seen[cache] = true
			out = append(out, weaveManagedGOCacheSweep{Issue: it.ID, Path: cache})
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cache := filepath.Join(root, entry.Name())
		issueID, ok := weaveManagedGOCacheIssue(absQueue, cache)
		if !ok || seen[cache] {
			continue
		}
		it := byIssue[issueID]
		if it == nil {
			seen[cache] = true
			out = append(out, weaveManagedGOCacheSweep{Issue: issueID, Path: cache, Orphan: true})
			continue
		}
		if weavePrunableForSweep(it.State, stale) {
			seen[cache] = true
			out = append(out, weaveManagedGOCacheSweep{Issue: it.ID, Path: cache})
			continue
		}
		if (it.State == "working" && it.WrapperPid > 0 && pidAlive(it.WrapperPid)) ||
			(it.State == "finalizing" && it.FinalizerPID > 0 && pidAlive(it.FinalizerPID)) {
			continue
		}
		if isTerminalState(it.State) {
			seen[cache] = true
			out = append(out, weaveManagedGOCacheSweep{Issue: it.ID, Path: cache})
		}
	}
	return out, nil
}

func envHasName(env []string, name string) bool {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// weaveReleaseManagedGOCache releases a run's managed build cache on EVERY
// terminal transition, whether or not a verify command ran.
//
// The cache is reproducible by construction (it is a GOCACHE), so nothing is
// lost by removing it, and it was the single largest artifact in the measured
// incident: one submitted run with no verify command held 6.1 GiB, because the
// release used to hang off the verify verdict. Failure is recorded on the row as
// CleanupError, not only printed — the transition is over by the time anyone
// reads stderr, and hygiene needs a fact it can refuse on. Success clears a
// stale CleanupError so a retry that worked stops reporting the old failure.
func weaveReleaseManagedGOCache(stderr io.Writer, verb, queueDir string, it *weaveItem) {
	if it == nil || queueDir == "" {
		return
	}
	err := weaveCleanupManagedGOCache(queueDir, it)
	msg := ""
	if err != nil {
		msg = fmt.Sprintf("managed GOCACHE: %v", err)
		fmt.Fprintf(stderr, "%s: managed GOCACHE cleanup failed (continuing): %v\n", verb, err)
	}
	if it.CleanupError == msg {
		return
	}
	it.CleanupError = msg
	_ = withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		if fresh := findWeaveItem(q, it.ID); fresh != nil {
			fresh.CleanupError = msg
		}
		return nil
	})
}
