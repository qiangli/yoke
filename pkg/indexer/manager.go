package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/execlog"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

type Root struct {
	Path       string    `json:"path"`
	Explicit   bool      `json:"explicit"`
	Visits     []string  `json:"visits,omitempty"`
	LastVisit  time.Time `json:"last_visit"`
	State      string    `json:"state"`
	Reason     string    `json:"reason"`
	Documents  int       `json:"documents"`
	LastUpdate time.Time `json:"last_update"`
	LastError  string    `json:"last_error,omitempty"`
}
type fingerprint struct {
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	ID       string `json:"id,omitempty"`
}
type Config struct {
	PendingTrim   map[string]int         `json:"pending_vector_trim,omitempty"`
	Phase         string                 `json:"phase,omitempty"`
	Version       int                    `json:"schema_version"`
	Enabled       bool                   `json:"enabled"`
	Paused        bool                   `json:"paused"`
	PromotionDays int                    `json:"promotion_days"`
	WindowDays    int                    `json:"window_days"`
	UnwatchDays   int                    `json:"unwatch_days"`
	PruneDays     int                    `json:"prune_days"`
	MaxRoots      int                    `json:"max_roots"`
	MaxFiles      int                    `json:"max_files"`
	MaxFileBytes  int64                  `json:"max_file_bytes"`
	Model         string                 `json:"embed_model,omitempty"`
	Roots         map[string]*Root       `json:"roots"`
	Files         map[string]fingerprint `json:"files,omitempty"`
	JournalSince  time.Time              `json:"journal_since,omitempty"`
	LastSync      time.Time              `json:"last_sync"`
	LastError     string                 `json:"last_error,omitempty"`
}
type Status struct {
	Phase         string    `json:"phase,omitempty"`
	SchemaVersion string    `json:"schema_version"`
	Enabled       bool      `json:"enabled"`
	Paused        bool      `json:"paused"`
	Roots         []*Root   `json:"roots"`
	Documents     int       `json:"documents"`
	LastSync      time.Time `json:"last_sync"`
	LastError     string    `json:"last_error,omitempty"`
	Model         string    `json:"embed_model,omitempty"`
	Limits        Config    `json:"limits"`
}
type Manager struct {
	dir   string
	Now   func() time.Time
	cfg   Config
	Embed kbmemory.Embedder
}

func New(home string) *Manager { return &Manager{dir: filepath.Join(home, "index"), Now: time.Now} }
func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
func (m *Manager) maxFiles() int   { return m.cfg.MaxFiles }
func (m *Manager) maxBytes() int64 { return m.cfg.MaxFileBytes }
func defaults() Config {
	return Config{Version: 1, Enabled: true, PromotionDays: 3, WindowDays: 14, UnwatchDays: 30, PruneDays: 90, MaxRoots: 64, MaxFiles: 1000, MaxFileBytes: 8 << 20, Roots: map[string]*Root{}, Files: map[string]fingerprint{}}
}
func (m *Manager) load() error {
	b, e := os.ReadFile(filepath.Join(m.dir, "state.json"))
	if e != nil {
		return fmt.Errorf("index: initialize explicitly with search index init: %w", e)
	}
	if e = json.Unmarshal(b, &m.cfg); e != nil {
		return e
	}
	m.cfg.Paused = fileExists(filepath.Join(m.dir, "paused"))
	m.cfg.Enabled = !fileExists(filepath.Join(m.dir, "disabled"))
	if e := validateConfig(m.cfg); e != nil {
		return e
	}
	if m.cfg.Roots == nil {
		m.cfg.Roots = map[string]*Root{}
	}
	if m.cfg.Files == nil {
		m.cfg.Files = map[string]fingerprint{}
	}
	return nil
}
func validateConfig(c Config) error {
	if c.Version != 1 || c.PromotionDays < 1 || c.WindowDays < c.PromotionDays || c.UnwatchDays < 1 || c.PruneDays <= c.UnwatchDays || c.MaxRoots < 1 || c.MaxRoots > 256 || c.MaxFiles < 1 || c.MaxFiles > kbmemory.MaxSyncRecords || c.MaxFileBytes < 1 || c.MaxFileBytes > 32<<20 {
		return fmt.Errorf("index: invalid configuration")
	}
	return nil
}
func (m *Manager) save() error {
	b, e := json.MarshalIndent(m.cfg, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(m.dir, ".state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), filepath.Join(m.dir, "state.json"))
}
func (m *Manager) locked(fn func() error) error {
	// Missing homes stay missing on reads/visits. Only Init creates the directory.
	if _, e := os.Stat(m.dir); e != nil {
		return e
	}
	l, e := lockfile.AcquireWithin(filepath.Join(m.dir, "state.lock"), wait, lockfile.Holder{Name: "index", PID: os.Getpid(), Intent: "index state/ingestion"})
	if e != nil {
		return e
	}
	defer l.Release()
	if e = m.load(); e != nil {
		return e
	}
	return fn()
}
func (m *Manager) Init() error {
	if st, e := os.Lstat(m.dir); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("index: state directory cannot be a symlink")
	}
	if e := os.MkdirAll(m.dir, 0700); e != nil {
		return e
	}
	if e := secureDirectory(m.dir); e != nil {
		return e
	}
	l, e := lockfile.AcquireWithin(filepath.Join(m.dir, "state.lock"), wait, lockfile.Holder{Name: "index", PID: os.Getpid()})
	if e != nil {
		return e
	}
	defer l.Release()
	if _, e = os.Stat(filepath.Join(m.dir, "state.json")); e == nil {
		return m.load()
	} else if !os.IsNotExist(e) {
		return e
	}
	if e = m.initStores(context.Background()); e != nil {
		return e
	}
	if docs, e := m.snapshot(context.Background()); e != nil {
		return e
	} else if len(docs) > 0 {
		return fmt.Errorf("index: existing documents have no root state; restore state or select a fresh home")
	}
	m.cfg = defaults()
	return m.save()
}
func canonical(path string) (string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	p, e = filepath.EvalSymlinks(p)
	if e != nil {
		return "", e
	}
	st, e := os.Stat(p)
	if e != nil {
		return "", e
	}
	if !st.IsDir() {
		return "", fmt.Errorf("index: root must be a directory")
	}
	return filepath.Clean(p), nil
}
func (m *Manager) Add(path string) error {
	return m.locked(func() error {
		p, e := canonical(path)
		if e != nil {
			return e
		}
		if m.badRoot(p, true) {
			return fmt.Errorf("index: excluded root %s", p)
		}
		if rr := repoRoot(p); rr != "" {
			p = rr
		}
		if m.cfg.Roots[p] == nil && len(m.cfg.Roots) >= m.cfg.MaxRoots {
			return fmt.Errorf("index: root cap %d", m.cfg.MaxRoots)
		}
		r := m.cfg.Roots[p]
		if r == nil {
			r = &Root{Path: p}
			m.cfg.Roots[p] = r
		}
		r.Explicit = true
		r.LastVisit = m.now()
		r.State = "watching"
		r.Reason = "explicit"
		return m.save()
	})
}
func (m *Manager) Forget(path string) error {
	p, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	if resolved, e := filepath.EvalSymlinks(p); e == nil {
		p = resolved
	}
	if rr := repoRoot(p); rr != "" {
		p = rr
	}
	return m.locked(func() error {
		delete(m.cfg.Roots, filepath.Clean(p))
		docs, err := m.snapshot(context.Background())
		if err != nil {
			return err
		}
		for _, d := range docs {
			var rec kbmemory.Record
			if err = json.Unmarshal(d.Doc, &rec); err != nil {
				return err
			}
			source, err := sourcePath(rec.Source.Ref)
			if err != nil {
				return err
			}
			keep := false
			for _, r := range m.cfg.Roots {
				if r.State != "candidate" && r.State != "pruned" && inside(r.Path, source) {
					keep = true
					break
				}
			}
			if !keep {
				if err = m.remove(context.Background(), d); err != nil {
					return err
				}
				delete(m.cfg.Files, source)
			}
		}
		if err = m.syncIndex(context.Background()); err != nil {
			return err
		}
		return m.save()
	})
}
func (m *Manager) visit(path string, at time.Time) {
	p, e := canonical(path)
	if e != nil || m.badRoot(p, false) {
		return
	}
	if rr := repoRoot(p); rr != "" {
		p = rr
	}
	m.observe(p, at)
}
func (m *Manager) observe(p string, at time.Time) {
	r := m.cfg.Roots[p]
	if r == nil {
		if len(m.cfg.Roots) >= m.cfg.MaxRoots {
			return
		}
		r = &Root{Path: p, State: "candidate", Reason: "visits"}
		m.cfg.Roots[p] = r
	}
	if at.After(r.LastVisit) {
		r.LastVisit = at
	}
	day := at.UTC().Format("2006-01-02")
	found := false
	for _, d := range r.Visits {
		if d == day {
			found = true
		}
	}
	if !found {
		r.Visits = append(r.Visits, day)
	}
	m.decay()
}
func (m *Manager) Visit(path string, at time.Time) error {
	return m.locked(func() error { m.visit(path, at); return m.save() })
}
func (m *Manager) decay() {
	now := m.now()
	for _, r := range m.cfg.Roots {
		keep := r.Visits[:0]
		for _, d := range r.Visits {
			t, e := time.Parse("2006-01-02", d)
			if e == nil && !t.Before(now.AddDate(0, 0, -m.cfg.WindowDays)) {
				keep = append(keep, d)
			}
		}
		r.Visits = keep
		if r.Explicit {
			r.State = "watching"
			r.Reason = "explicit"
			continue
		}
		switch {
		case now.Sub(r.LastVisit) >= time.Duration(m.cfg.PruneDays)*24*time.Hour:
			r.State = "pruned"
		case now.Sub(r.LastVisit) >= time.Duration(m.cfg.UnwatchDays)*24*time.Hour:
			r.State = "unwatched"
		case r.State == "watching" || len(r.Visits) >= m.cfg.PromotionDays:
			r.State = "watching"
		default:
			r.State = "candidate"
		}
		r.Reason = fmt.Sprintf("visits: %d distinct days in %d", len(r.Visits), m.cfg.WindowDays)
	}
}
func fileExists(p string) bool { _, e := os.Stat(p); return e == nil }

// Tiny owner-only control signals remain writable while a bounded ingest/model
// operation holds the state transaction. They never create or initialize a home.
func (m *Manager) setControl(name string, on bool) error {
	m = &Manager{dir: m.dir, Now: m.Now, Embed: m.Embed}
	if e := m.load(); e != nil {
		return e
	}
	p := filepath.Join(m.dir, name)
	if on {
		return os.WriteFile(p, []byte("1\n"), 0600)
	}
	e := os.Remove(p)
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
func (m *Manager) SetPaused(paused bool) error   { return m.setControl("paused", paused) }
func (m *Manager) SetEnabled(enabled bool) error { return m.setControl("disabled", !enabled) }
func (m *Manager) stopped() bool {
	return fileExists(filepath.Join(m.dir, "paused")) || fileExists(filepath.Join(m.dir, "disabled"))
}

// Configure changes only validated limits; roots and source manifests remain owned by the service.
func (m *Manager) Configure(changes Config) error {
	return m.locked(func() error {
		c := m.cfg
		if changes.PromotionDays != 0 {
			c.PromotionDays = changes.PromotionDays
		}
		if changes.WindowDays != 0 {
			c.WindowDays = changes.WindowDays
		}
		if changes.UnwatchDays != 0 {
			c.UnwatchDays = changes.UnwatchDays
		}
		if changes.PruneDays != 0 {
			c.PruneDays = changes.PruneDays
		}
		if changes.MaxRoots != 0 {
			c.MaxRoots = changes.MaxRoots
		}
		if changes.MaxFiles != 0 {
			c.MaxFiles = changes.MaxFiles
		}
		if changes.MaxFileBytes != 0 {
			c.MaxFileBytes = changes.MaxFileBytes
		}
		if e := validateConfig(c); e != nil {
			return e
		}
		m.cfg = c
		return m.save()
	})
}
func (m *Manager) Status() (Status, error) {
	m = &Manager{dir: m.dir, Now: m.Now, Embed: m.Embed}
	if e := m.load(); e != nil {
		return Status{}, e
	}
	out := Status{SchemaVersion: "bashy-index-v1", Phase: m.cfg.Phase, Enabled: m.cfg.Enabled, Paused: m.cfg.Paused, LastSync: m.cfg.LastSync, LastError: m.cfg.LastError, Model: m.cfg.Model, Limits: m.cfg, Roots: []*Root{}}
	out.Limits.Roots = nil
	out.Limits.Files = nil
	for _, r := range m.cfg.Roots {
		c := *r
		out.Roots = append(out.Roots, &c)
		out.Documents += r.Documents
	}
	sort.Slice(out.Roots, func(i, j int) bool { return out.Roots[i].Path < out.Roots[j].Path })
	return out, nil
}

func (m *Manager) consumeJournal() {
	since := m.cfg.JournalSince
	if since.IsZero() {
		since = m.now().AddDate(0, 0, -m.cfg.WindowDays)
	}
	records, _, e := execlog.Read(filepath.Join(filepath.Dir(m.dir), "exec"), execlog.Query{Since: since, Limit: 10000})
	if e != nil {
		return
	}
	for _, r := range records {
		if !r.At.After(m.now()) {
			m.visit(r.Cwd, r.At)
			if r.At.After(m.cfg.JournalSince) {
				m.cfg.JournalSince = r.At
			}
		}
	}
}

var excludedNames = map[string]bool{"AppData": true, "Application Data": true, "Local Settings": true, "appdata": true, "library": true, "node_modules": true, "vendor": true, "venv": true, "build": true, "dist": true, "target": true, "Library": true, "tmp": true, "temp": true, "cache": true, "caches": true, "keychains": true, "passwords": true, "credentials": true, "models": true, "__pycache__": true, "System Volume Information": true}

func (m *Manager) badRoot(p string, explicit bool) bool {

	if p == filepath.VolumeName(p)+string(filepath.Separator) {
		return true
	}
	h, _ := os.UserHomeDir()
	if p == h && !explicit {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(part, ".") || excludedNames[part] {
			return true
		}
	}
	temp := filepath.Clean(os.TempDir())
	if resolved, e := filepath.EvalSymlinks(temp); e == nil {
		temp = resolved
	}
	if inside(temp, p) {
		return true
	}
	return inside(m.dir, p)
}
func inside(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
}
func (m *Manager) excluded(root, path string, st fs.FileInfo) bool {
	if placeholder(st) || st.Mode()&os.ModeSymlink != 0 || inside(m.dir, path) {
		return true
	}
	rel, e := filepath.Rel(root, path)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		low := strings.ToLower(part)
		if strings.HasPrefix(part, ".") || excludedNames[part] || excludedNames[low] {
			return true
		}
		for _, suffix := range []string{".app", ".photoslibrary", ".photolibrary", ".pvm", ".vmdk", ".qcow2", ".vdi", ".pem", ".key", ".p12", ".pfx", ".kdbx", ".gguf", ".safetensors"} {
			if strings.HasSuffix(low, suffix) {
				return true
			}
		}
		if low == "id_rsa" || low == "id_ed25519" || strings.HasPrefix(low, "credentials.") {
			return true
		}
	}
	return false
}
