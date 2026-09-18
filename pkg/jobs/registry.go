package jobs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AppName is the application directory name used by DefaultJobsDir.
// Embedders may set this during process startup before calling DefaultRegistry.
var AppName = "bashy"

// JobRecord is one row in the persistent background-job registry. PID is both
// the kernel PID and the filename key on disk.
type JobRecord struct {
	PID       int       `json:"pid"`
	User      string    `json:"user"`
	Cmd       string    `json:"cmd"`
	StartedAt time.Time `json:"started_at"`
}

// JobRegistry persists detached background jobs. It stores one JSON file per
// job at <dir>/<pid>.json.
type JobRegistry struct {
	dir string // empty = no-op (UserCacheDir not available)
}

// NewJobRegistry constructs a registry rooted at dir. Pass "" to disable it.
func NewJobRegistry(dir string) *JobRegistry { return &JobRegistry{dir: dir} }

// DefaultJobsDir is the path the default registry writes to. The registry
// creates it lazily on first Record.
func DefaultJobsDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, AppName, "jobs"), nil
}

var defaultRegistry = sync.OnceValue(func() *JobRegistry {
	dir, _ := DefaultJobsDir()
	return &JobRegistry{dir: dir}
})

// DefaultRegistry is the process-wide registry rooted at DefaultJobsDir.
func DefaultRegistry() *JobRegistry { return defaultRegistry() }

// Record inserts a job for the given kernel PID.
func (r *JobRegistry) Record(pid int, cmd string) error {
	if r.dir == "" {
		return errors.New("jobs: no cache dir")
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	rec := JobRecord{
		PID:       pid,
		User:      currentOSUser(),
		Cmd:       cmd,
		StartedAt: time.Now().UTC(),
	}
	return writeRecord(filepath.Join(r.dir, strconv.Itoa(pid)+".json"), rec)
}

// List returns all currently-recorded jobs, sorted by PID, with dead records
// pruned in-place.
func (r *JobRegistry) List() ([]JobRecord, error) {
	if r.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rows []JobRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		rec, err := readRecord(path)
		if err != nil {
			continue
		}
		if !pidAlive(rec.PID) {
			_ = os.Remove(path)
			continue
		}
		rows = append(rows, rec)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PID < rows[j].PID })
	return rows, nil
}

// Get returns one record. It returns fs.ErrNotExist if pid is not registered.
func (r *JobRegistry) Get(pid int) (JobRecord, error) {
	if r.dir == "" {
		return JobRecord{}, fs.ErrNotExist
	}
	return readRecord(filepath.Join(r.dir, strconv.Itoa(pid)+".json"))
}

// Delete removes the registry entry for pid. It has no effect on the process.
func (r *JobRegistry) Delete(pid int) error {
	if r.dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(r.dir, strconv.Itoa(pid)+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func writeRecord(path string, rec JobRecord) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func readRecord(path string) (JobRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return JobRecord{}, err
	}
	defer f.Close()
	var rec JobRecord
	return rec, json.NewDecoder(f).Decode(&rec)
}

func currentOSUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}
