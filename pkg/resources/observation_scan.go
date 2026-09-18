package resources

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const maxObservationScanEntries = 10000
const maxObservationScanRoots = 32

type directoryScanBudget struct{ remaining int }

// scanObservationDirectory streams entries, never opens file contents or follows
// symlinks, and shares one entry/deadline budget across all roots in a refresh.
func scanObservationDirectory(ctx context.Context, root string, at time.Time, budget *directoryScanBudget) DirectoryObservation {
	result := DirectoryObservation{Path: root, Coverage: ObservationCoverage{Complete: true, Limit: maxObservationScanEntries}, Bytes: unknownValue[uint64]("directory:apparent-bytes", "not scanned", at), GrowthBytesPerSecond: unknownValue[float64]("directory:delta", "requires two complete comparable scans", at)}
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		result.Coverage.Complete = false
		result.Coverage.Reason = "symlink root not followed"
		result.Bytes.Status.Reason = result.Coverage.Reason
		return result
	}
	total := uint64(0)
	var visit func(string, int) error
	visit = func(path string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 64 {
			return fmt.Errorf("directory depth limit")
		}
		if budget.remaining <= 0 {
			return fmt.Errorf("directory entry limit")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		budget.remaining--
		result.Entries++
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.Mode().IsRegular() {
			if info.Size() > 0 {
				if uint64(info.Size()) > ^uint64(0)-total {
					return fmt.Errorf("directory byte count overflow")
				}
				total += uint64(info.Size())
			}
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(info, opened) {
			return fmt.Errorf("directory changed while opening")
		}
		for {
			entries, readErr := f.ReadDir(128)
			for _, entry := range entries {
				if err := visit(filepath.Join(path, entry.Name()), depth+1); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	err := visit(root, 0)
	result.Coverage.Seen = result.Entries
	result.Coverage.Included = result.Entries
	if err != nil {
		result.Coverage.Complete = false
		result.Coverage.Reason = err.Error()
	}
	if result.Entries > 0 {
		result.Bytes = observedValue(total, "directory:apparent-bytes", at, DirectoryObservationTTL)
		if err != nil {
			result.Bytes.Status.Kind = "estimated"
			result.Bytes.Status.Reason = "partial lower bound: " + err.Error()
		}
	} else {
		result.Bytes.Status = observationStatus("unknown", "directory:apparent-bytes", result.Coverage.Reason, at, DirectoryObservationTTL)
	}
	result.GrowthBytesPerSecond.Status.ExpiresAt = at.Add(DirectoryObservationTTL)
	return result
}
func directoryGrowth(current *DirectoryObservation, prior DirectoryObservation) {
	elapsed := current.Bytes.Status.At.Sub(prior.Bytes.Status.At).Seconds()
	if !current.Coverage.Complete || !prior.Coverage.Complete || current.Bytes.Value == nil || prior.Bytes.Value == nil || elapsed <= 0 || elapsed > 600 {
		return
	}
	v := (float64(*current.Bytes.Value) - float64(*prior.Bytes.Value)) / elapsed
	current.GrowthBytesPerSecond = observedValue(v, "directory:apparent-byte-delta", current.Bytes.Status.At, DirectoryObservationTTL)
	current.GrowthBytesPerSecond.Status.Kind = "estimated"
	current.GrowthBytesPerSecond.Status.Reason = "path-level change, not proof of exclusive allocation or reclaimable bytes"
	current.GrowthBytesPerSecond.Status.WindowStart = prior.Bytes.Status.At
	current.GrowthBytesPerSecond.Status.WindowEnd = current.Bytes.Status.At
}
