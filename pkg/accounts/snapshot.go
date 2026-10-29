package accounts

import (
	"errors"
	"sort"
	"time"
)

// Snapshot is the per-account cache written by `account refresh` and read by
// `account usage` and `account resources`. Reads are cache-only: usage and
// resources never call adapters, they only render the snapshot and its age.
type Snapshot struct {
	AccountID  string              `json:"account_id"`
	Resources  []ResourceRef       `json:"resources,omitempty"`
	Metrics    map[string][]Metric `json:"metrics,omitempty"`
	ObservedAt time.Time           `json:"observed_at"`
	LastError  string              `json:"last_error,omitempty"`
}

// Snapshot returns the cached refresh snapshot for one account, or
// ErrNotFound when the account was never refreshed.
func (s *Store) Snapshot(id string) (Snapshot, error) {
	d, err := s.read()
	if err != nil {
		return Snapshot{}, err
	}
	for _, snap := range d.Snapshots {
		if snap.AccountID == id {
			return snap, nil
		}
	}
	return Snapshot{}, ErrNotFound
}

// SaveSnapshot replaces the cached snapshot for one account. A successful
// refresh clears any previous error.
func (s *Store) SaveSnapshot(snap Snapshot) error {
	if snap.AccountID == "" {
		return errors.New("accounts: snapshot needs an account id")
	}
	if snap.LastError != "" && !errorCodePattern.MatchString(snap.LastError) {
		return errors.New("accounts: last_error must be a safe code")
	}
	return s.mutate(func(d *document) error {
		for i := range d.Snapshots {
			if d.Snapshots[i].AccountID == snap.AccountID {
				d.Snapshots[i] = snap
				return nil
			}
		}
		d.Snapshots = append(d.Snapshots, snap)
		return nil
	})
}

// RecordSnapshotError marks the snapshot of one account failed without
// touching its last-good resources and metrics. The raw adapter error is
// never persisted: only a safe code is kept, so credential material in error
// text cannot reach the store file.
func (s *Store) RecordSnapshotError(id, code string, at time.Time) error {
	if id == "" {
		return errors.New("accounts: snapshot needs an account id")
	}
	if code == "" || !errorCodePattern.MatchString(code) {
		return errors.New("accounts: last_error must be a safe code")
	}
	return s.mutate(func(d *document) error {
		for i := range d.Snapshots {
			if d.Snapshots[i].AccountID == id {
				d.Snapshots[i].LastError = code
				d.Snapshots[i].ObservedAt = at
				return nil
			}
		}
		d.Snapshots = append(d.Snapshots, Snapshot{AccountID: id, LastError: code, ObservedAt: at})
		return nil
	})
}

func sortSnapshots(snaps []Snapshot) {
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].AccountID < snaps[j].AccountID })
}
