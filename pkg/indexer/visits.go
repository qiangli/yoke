package indexer

import (
	"bufio"
	"encoding/json"
	"errors"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"io"
	"os"
	"path/filepath"
	"time"
)

type directoryVisit struct {
	Path string    `json:"path"`
	At   time.Time `json:"at"`
}

// RecordVisit writes only a directory and timestamp. It is best-effort,
// non-blocking. It may create the owner-only directory journal, never stores,
// configuration or a service. No argv is retained.
func RecordVisit(home, path string, at time.Time) bool {
	m := New(home)
	p, e := canonical(path)
	if e != nil || m.badRoot(p, false) {
		return false
	}
	if e := m.load(); e != nil {
		if !errors.Is(e, os.ErrNotExist) || fileExists(filepath.Join(m.dir, "disabled")) {
			return false
		}
		if st, e := os.Lstat(m.dir); e == nil && st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if os.MkdirAll(m.dir, 0700) != nil || secureDirectory(m.dir) != nil {
			return false
		}
	} else if !m.cfg.Enabled {
		return false
	}
	l, e := lockfile.TryAcquire(filepath.Join(m.dir, "visits.lock"), lockfile.Holder{Name: "directory visit", PID: os.Getpid()})
	if e != nil {
		return false
	}
	defer l.Release()
	f, e := os.OpenFile(filepath.Join(m.dir, "visits.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return false
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() > 4<<20 {
		return false
	}
	b, e := json.Marshal(directoryVisit{Path: p, At: at.UTC()})
	if e != nil || len(b) > 4095 {
		return false
	}
	_, e = f.Write(append(b, '\n'))
	return e == nil
}
func (m *Manager) consumeVisits() {
	l, e := lockfile.TryAcquire(filepath.Join(m.dir, "visits.lock"), lockfile.Holder{Name: "consume directory visits", PID: os.Getpid()})
	if e != nil {
		return
	}
	defer l.Release()
	f, e := os.OpenFile(filepath.Join(m.dir, "visits.jsonl"), os.O_RDWR, 0600)
	if e != nil {
		return
	}
	defer f.Close()
	// Producers cap this small journal. The state transaction already holds
	// the root lock; consuming and truncating it cannot interleave an append.
	scan := bufio.NewScanner(io.LimitReader(f, 4<<20))
	scan.Buffer(make([]byte, 4096), 8192)
	for scan.Scan() {
		var v directoryVisit
		if json.Unmarshal(scan.Bytes(), &v) == nil && !v.At.After(m.now()) && v.At.After(m.now().AddDate(0, 0, -m.cfg.WindowDays)) {
			m.visit(v.Path, v.At)
		}
	}
	if scan.Err() == nil && m.save() == nil {
		_ = f.Truncate(0)
	}
}
