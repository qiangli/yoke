package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"io"
	"os"
	"path/filepath"
	"time"
)

func observationDir(dir string) (string, error) {
	if dir == "" {
		dir = ResourcesStateDir()
	}
	if dir == "" {
		return "", errors.New("resources: no state directory")
	}
	return dir, nil
}
func readObservationJSON(path string, limit int64, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return fmt.Errorf("resources: state is not a regular file within %d bytes", limit)
	}
	// Atomic state files are immutable through this descriptor. Allocate once
	// from its bounded size; ReadAll repeatedly grew/copies large host caches.
	b := make([]byte, int(info.Size())+1)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	if int64(n) > info.Size() {
		return errors.New("resources: state changed while reading")
	}
	return json.Unmarshal(b[:n], value)
}
func writeObservationJSON(path string, limit int, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > limit {
		return fmt.Errorf("resources: state exceeds %d bytes", limit)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".observation-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func observationLock(ctx context.Context, path string) (*lockfile.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := lockfile.TryAcquire(path, lockfile.Holder{Name: "resources", Intent: "derived state"})
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, lockfile.ErrHeld) {
			return nil, err
		}
		if err := sleepCtx(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

const maxAlertStateBytes = 2 << 20

func ReadAlertState(ctx context.Context, dir string) (*AlertLedger, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := observationDir(dir)
	if err != nil {
		return nil, err
	}
	state := &AlertLedger{SchemaVersion: AlertStateSchema, Entries: map[string]json.RawMessage{}}
	err = readObservationJSON(filepath.Join(dir, "alerts.json"), maxAlertStateBytes, state)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validateAlertState(state); err != nil {
		return nil, err
	}
	if state.Entries == nil {
		state.Entries = map[string]json.RawMessage{}
	}
	return state, nil
}
func validateAlertState(state *AlertLedger) error {
	if state.SchemaVersion != AlertStateSchema {
		return errors.New("resources: unsupported alert state schema")
	}
	if len(state.Entries) > 256 {
		return errors.New("resources: alert state exceeds 256 entries")
	}
	for key, value := range state.Entries {
		if key == "" || len(key) > 256 || len(value) > 8192 || !json.Valid(value) {
			return errors.New("resources: invalid or oversized alert entry")
		}
	}
	return nil
}

// UpdateAlertState serializes one short, in-memory derived-state transaction.
// The callback must not perform IO or delivery. Errors leave disk unchanged.
func UpdateAlertState(ctx context.Context, dir string, update func(*AlertLedger) error) (*AlertLedger, error) {
	dir, err := observationDir(dir)
	if err != nil {
		return nil, err
	}
	if update == nil {
		return nil, errors.New("resources: missing alert transaction")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := observationLock(ctx, filepath.Join(dir, ".alerts.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	state, err := ReadAlertState(ctx, dir)
	if err != nil {
		return nil, err
	}
	revision := state.Revision
	updatedAt := state.UpdatedAt
	// Entries are already bounded, validated JSON. Compare their bytes directly
	// rather than serializing and reformatting the entire ledger twice on every
	// client poll. Copy the bytes too: callbacks may mutate RawMessage in place.
	before := make(map[string]json.RawMessage, len(state.Entries))
	total := 0
	for _, value := range state.Entries {
		total += len(value)
	}
	snapshot := make([]byte, total)
	for key, value := range state.Entries {
		n := copy(snapshot, value)
		before[key] = snapshot[:n:n]
		snapshot = snapshot[n:]
	}
	if err := update(state); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAlertState(state); err != nil {
		return nil, err
	}
	unchanged := state.Entries != nil && len(before) == len(state.Entries)
	if unchanged {
		for key, value := range before {
			next, ok := state.Entries[key]
			if !ok || !bytes.Equal(value, next) {
				unchanged = false
				break
			}
		}
	}
	if unchanged {
		state.Revision = revision
		state.UpdatedAt = updatedAt
		return state, nil
	}
	state.Revision = revision + 1
	state.UpdatedAt = time.Now().UTC()
	if err := validateAlertState(state); err != nil {
		return nil, err
	}
	if err := writeObservationJSON(filepath.Join(dir, "alerts.json"), maxAlertStateBytes, state); err != nil {
		return nil, err
	}
	return state, nil
}
