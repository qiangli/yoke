package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

func (m *Manager) Sync(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if m.stopped() {
					cancel()
					return
				}
			}
		}
	}()

	return m.locked(func() (result error) {
		defer func() {
			if result != nil {
				m.cfg.Phase = "idle"
				m.cfg.LastError = result.Error()
				if m.stopped() {
					m.cfg.LastError = ""
					result = nil
				}
				if e := m.save(); e != nil {
					result = errors.Join(result, e)
				}
			}
		}()

		if !m.cfg.Enabled || m.cfg.Paused {
			return nil
		}
		m.cfg.Phase = "extracting"
		if e := m.save(); e != nil {
			return e
		}
		m.consumeJournal()
		m.consumeVisits()
		m.decay()
		old, e := m.snapshot(ctx)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		next := map[string]fingerprint{}
		remotes := map[string]string{}
		roots := make([]string, 0, len(m.cfg.Roots))
		for p := range m.cfg.Roots {
			roots = append(roots, p)
		}
		sort.Strings(roots)
		totalBytes, totalFiles := 0, 0
		var rootErrors []string
		for _, p := range roots {
			if e = ctx.Err(); e != nil {
				return e
			}
			r := m.cfg.Roots[p]
			r.Documents = 0
			r.LastError = ""
			if r.State == "candidate" || r.State == "pruned" {
				continue
			}
			if r.State == "unwatched" {
				r.Documents = preserveRoot(old, seen, p)
				continue
			}
			files, kind, err := m.files(p, remotes)
			if err != nil {
				r.LastError = err.Error()
				rootErrors = append(rootErrors, r.Path+": "+err.Error())
				r.Documents = preserveRoot(old, seen, p)
				continue
			}
			if strings.HasPrefix(kind, "duplicate of ") {
				r.Reason = kind
				continue
			}
			for _, f := range files {
				if e = ctx.Err(); e != nil {
					return e
				}
				id := sourceID(f.unit, f.rel)
				st, err := os.Lstat(f.path)
				if err != nil {
					r.LastError = err.Error()
					continue
				}
				fp := fingerprint{Size: st.Size(), Modified: st.ModTime().UnixNano()}
				if prior, ok := m.cfg.Files[f.path]; ok && prior.Size == fp.Size && prior.Modified == fp.Modified && prior.ID != "" {
					id = prior.ID
				}
				ext := strings.ToLower(filepath.Ext(f.path))
				archive := ext == ".zip" || ext == ".tar" || ext == ".tgz" || ext == ".jar" || ext == ".whl" || strings.HasSuffix(strings.ToLower(f.path), ".tar.gz")
				if archive && id == sourceID(f.unit, f.rel) {
					raw, err := m.readFile(f.root, f.path)
					if err != nil {
						r.LastError = err.Error()
						continue
					}
					hash := sha256.Sum256(raw)
					id = sourceID("archive", hex.EncodeToString(hash[:]))
				}
				fp.ID = id
				if seen[id] {
					continue
				}
				prev, exists := old[id]
				if previous, ok := m.cfg.Files[f.path]; ok && previous == fp && exists {
					seen[id] = true
					next[f.path] = fp
					r.Documents++
					totalFiles++
					totalBytes += len(prev.Doc)
					continue
				}
				text, err := m.extract(ctx, f.root, f.path)
				if err != nil {
					r.LastError = fmt.Sprintf("%s: %v", filepath.Base(f.path), err)
					continue
				}
				if strings.TrimSpace(text) == "" {
					continue
				}
				totalBytes += len(text) + 1024
				totalFiles++
				if totalFiles > m.cfg.MaxFiles || totalBytes > 4<<20 {
					return fmt.Errorf("index: corpus cap exceeded (%d files, 4MiB extracted text)", m.cfg.MaxFiles)
				}
				var before *dbdoc.Document
				if exists {
					before = &prev
				}
				if err = m.put(ctx, id, f.path, text, before); err != nil {
					return err
				}
				seen[id] = true
				next[f.path] = fp
				r.Documents++
			}
			r.LastUpdate = m.now()
		}
		for id, d := range old {
			if !seen[id] {
				if e = m.remove(ctx, d); e != nil {
					return e
				}
			}
		}
		if e = m.syncIndex(ctx); e != nil {
			m.cfg.LastError = e.Error()
			_ = m.save()
			return e
		}
		m.cfg.Files = next
		m.cfg.Phase = "embedding"
		if m.cfg.Model == "" {
			m.cfg.Phase = "idle"
		}
		if e = m.save(); e != nil {
			return e
		}
		if e = m.syncVectors(ctx); e != nil {
			m.cfg.LastError = e.Error()
			_ = m.save()
			return e
		}
		if len(rootErrors) > 0 {
			return fmt.Errorf("index: incomplete roots: %s", strings.Join(rootErrors, "; "))
		}
		m.cfg.Phase = "idle"
		m.cfg.Files = next
		m.cfg.LastSync = m.now()
		m.cfg.LastError = ""
		return m.save()
	})
}
func preserveRoot(old map[string]dbdoc.Document, seen map[string]bool, root string) int {
	count := 0
	for id, d := range old {
		var rec kbmemory.Record
		if json.Unmarshal(d.Doc, &rec) == nil {
			path, e := sourcePath(rec.Source.Ref)
			if e == nil && inside(root, path) {
				seen[id] = true
				count++
			}
		}
	}
	return count
}

// Run combines native events for non-git roots with bounded repo-state polling.
// Event loss cannot strand the index: the periodic reconciliation is authoritative.
func (m *Manager) Run(ctx context.Context) error {
	if _, e := m.Status(); e != nil {
		return e
	}
	watcher, e := fsnotify.NewWatcher()
	if e != nil {
		return e
	}
	defer watcher.Close()
	rebuild := func() {
		st, e := m.Status()
		if e != nil {
			return
		}
		wanted := map[string]bool{}
		if st.Enabled && !st.Paused {
			for _, r := range st.Roots {
				if r.State != "watching" || repoRoot(r.Path) != "" {
					continue
				}
				_ = filepath.WalkDir(r.Path, func(p string, d os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if !d.IsDir() {
						return nil
					}
					info, err := d.Info()
					if err != nil {
						return filepath.SkipDir
					}
					if p != r.Path && (m.excluded(r.Path, p, info) || ignored(r.Path, p, true)) {
						return filepath.SkipDir
					}
					if p != r.Path {
						if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
							return filepath.SkipDir
						}
					}
					if len(wanted) >= 2048 {
						return filepath.SkipDir
					}
					wanted[p] = true
					return nil
				})
			}
		}
		for _, p := range watcher.WatchList() {
			if !wanted[p] {
				_ = watcher.Remove(p)
			} else {
				delete(wanted, p)
			}
		}
		for p := range wanted {
			_ = watcher.Add(p)
		}
	}
	sync := func() {
		if e := m.Sync(ctx); e != nil && ctx.Err() == nil {
			_ = m.locked(func() error { m.cfg.LastError = e.Error(); return m.save() })
		}
		rebuild()
	}
	sync()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			sync()
		case _, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(150 * time.Millisecond)
		case <-debounce.C:
			sync()
		case err, ok := <-watcher.Errors:
			if ok {
				_ = m.locked(func() error { m.cfg.LastError = err.Error(); return m.save() })
			}
		}
	}
}
