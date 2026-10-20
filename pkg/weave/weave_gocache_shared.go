package weave

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// weaveGOCacheHostLive reports whether any weave run on the host (any queue
// under stateRoot) is working or finalizing. Injectable for tests.
var weaveGOCacheHostLive = func(stateRoot string) bool {
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		q, err := loadWeaveQueue(filepath.Join(stateRoot, e.Name()))
		if err != nil || q == nil {
			continue
		}
		for _, it := range q.Items {
			if it == nil || !(it.State == "working" || it.State == "finalizing") {
				continue
			}
			if (it.WrapperPid == 0 && it.FinalizerPID == 0) ||
				(it.WrapperPid > 0 && pidAlive(it.WrapperPid)) ||
				(it.FinalizerPID > 0 && pidAlive(it.FinalizerPID)) {
				return true
			}
		}
	}
	return false
}

func weaveGOCacheMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv(weaveGOCacheMaxEnv)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return weaveGOCacheDefaultMax
}

// weaveTrimSharedGOCache evicts least-recently-used files from the shared cache
// until it is at or under the cap. It does nothing while any run on the host is
// live, and it removes only regular files, never directories: Go tolerates a
// missing cache entry (it rebuilds) but not a directory vanishing mid-write, so
// the cache root and its subdirectories are left in place. Returns bytes freed.
func weaveTrimSharedGOCache(queueDir string, maxBytes int64) (int64, error) {
	root := filepath.Join(filepath.Dir(queueDir), weaveSharedGOCacheDir)
	lst, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if lst.Mode()&os.ModeSymlink != 0 || !lst.IsDir() {
		return 0, nil
	}
	if weaveGOCacheHostLive(filepath.Dir(queueDir)) {
		return 0, nil
	}
	type ent struct {
		path string
		size int64
		mod  int64
	}
	var files []ent
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, ent{p, info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
		return nil
	})
	if total <= maxBytes {
		return 0, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	var freed int64
	for _, f := range files {
		if total-freed <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			freed += f.size
		}
	}
	return freed, nil
}

// weaveMigrateLegacyGOCaches removes per-run cache dirs left by older runs
// whose run is not live (safeRemoveManagedGOCache refuses live ones).
func weaveMigrateLegacyGOCaches(queueDir string) {
	q, err := loadWeaveQueue(queueDir)
	if err != nil || q == nil {
		return
	}
	targets, err := weaveManagedGOCacheSweepTargets(queueDir, q, false)
	if err != nil {
		return
	}
	for _, t := range targets {
		_ = safeRemoveManagedGOCache(queueDir, t.Path)
	}
}
