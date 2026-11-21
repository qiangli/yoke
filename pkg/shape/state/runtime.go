package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// Runtime describes a ready, console-managed server for one app principal.
type Runtime struct {
	Addr     string `json:"addr"`
	Data     string `json:"data"`
	URL      string `json:"url"`
	PID      int    `json:"pid"` // owning console process
	Revision string `json:"revision"`
}

func (s *Store) runtimePath() string { return filepath.Join(s.dir, "runtime.json") }

func (s *Store) ReadRuntime() (Runtime, error) {
	var r Runtime
	body, err := os.ReadFile(s.runtimePath())
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(body, &r)
	return r, err
}

func (s *Store) WriteRuntime(r Runtime) error {
	l, err := s.lockRuntime()
	if err != nil {
		return err
	}
	defer l.Release()
	if current, err := s.ReadRuntime(); err == nil && current == r {
		return nil
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicWrite(s.runtimePath(), body, 0o600)
}

// RemoveRuntime removes the record only if the exiting process still owns it.
func (s *Store) RemoveRuntime(r Runtime) error {
	l, err := s.lockRuntime()
	if err != nil {
		return err
	}
	defer l.Release()
	current, err := s.ReadRuntime()
	if errors.Is(err, os.ErrNotExist) || (err == nil && current != r) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(s.runtimePath())
}

func (s *Store) lockRuntime() (*lockfile.Lock, error) {
	// DataDir may already have created the principal directory.
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return nil, err
	}
	return lockfile.AcquireWithin(filepath.Join(s.dir, ".runtime.lock"), time.Second,
		lockfile.Holder{Intent: "shape runtime update"})
}
