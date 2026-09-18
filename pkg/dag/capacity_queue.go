package dag

import (
	"encoding/json"
	"errors"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"os"
	"path/filepath"
	"time"
)

type capacityQueuedRequest struct {
	Version int             `json:"version"`
	At      time.Time       `json:"at"`
	Reason  string          `json:"reason"`
	Request CapacityRequest `json:"request"`
}

// Fallback is a durable local request, never an implicit retry or local launch.
func queueCapacityRequest(r CapacityRequest, reason string) error {
	dir := filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", "pending")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	lock, e := lockfile.TryAcquire(filepath.Join(filepath.Dir(dir), "pending.lock"), lockfile.Holder{Name: "capacity-queue"})
	if e != nil {
		return e
	}
	defer lock.Release()
	path := filepath.Join(dir, capacityOutputDigest(r.ID)+".json")
	f, e := os.Open(path)
	if e == nil {
		var old capacityQueuedRequest
		e = decodeCapacity(f, &old)
		f.Close()
		if e != nil {
			return e
		}
		if capacityDigest(old.Request) != capacityDigest(r) {
			return errors.New("queued capacity ID reused with different request")
		}
		return nil
	}
	if !os.IsNotExist(e) {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	entries, _ := d.ReadDir(257)
	d.Close()
	if len(entries) >= 256 {
		return errors.New("local capacity queue full; request was not discarded or executed")
	}
	b, e := json.Marshal(capacityQueuedRequest{Version: 1, At: time.Now().UTC(), Reason: reason, Request: r})
	if e != nil {
		return e
	}
	f, e = os.CreateTemp(dir, ".pending-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
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
	return os.Rename(f.Name(), path)
}

func clearCapacityQueuedRequest(r CapacityRequest) error {
	dir := filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", "pending")
	path := filepath.Join(dir, capacityOutputDigest(r.ID)+".json")
	if _, e := os.Stat(path); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	}
	lock, e := lockfile.TryAcquire(filepath.Join(filepath.Dir(dir), "pending.lock"), lockfile.Holder{Name: "capacity-queue-complete"})
	if e != nil {
		return e
	}
	defer lock.Release()
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var old capacityQueuedRequest
	e = decodeCapacity(f, &old)
	f.Close()
	if e != nil {
		return e
	}
	if capacityDigest(old.Request) != capacityDigest(r) {
		return errors.New("completed request differs from pending request; retained for inspection")
	}
	return os.Remove(path)
}
