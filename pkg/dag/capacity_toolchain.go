package dag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CapacityExecutable identifies an explicitly declared executable artifact.
// It attests bytes available at Path, not libraries, compiler subprocesses,
// runtime-loaded code, or arbitrary descendants of that executable.
type CapacityExecutable struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const (
	capacityExecutableLimit  = 8
	capacityExecutableBytes  = 256 << 20
	capacityInventoryBytes   = 512 << 20
	capacityInventoryTimeout = 3 * time.Second
)

// VerifyCapacityExecutables verifies a bounded, explicit executable inventory
// without executing it. Callers must compare the declaration to receiver policy
// and verify again after execution before accepting returned provenance.
func VerifyCapacityExecutables(ctx context.Context, declared []CapacityExecutable) ([]CapacityExecutable, error) {
	if len(declared) > capacityExecutableLimit {
		return nil, errors.New("capacity: executable inventory exceeds eight entries")
	}
	ctx, cancel := context.WithTimeout(ctx, capacityInventoryTimeout)
	defer cancel()
	// Filesystem operations may stall (for example an unavailable mount).
	// Bound caller latency and cap outstanding filesystem workers globally.
	select {
	case capacityFingerprintWorkers <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		inventory []CapacityExecutable
		err       error
	}
	done := make(chan result, 1)
	input := append([]CapacityExecutable(nil), declared...)
	go func() {
		defer func() { <-capacityFingerprintWorkers }()
		inventory, err := verifyCapacityExecutables(ctx, input)
		done <- result{inventory, err}
	}()
	select {
	case result := <-done:
		return result.inventory, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var capacityFingerprintWorkers = make(chan struct{}, 8)

func verifyCapacityExecutables(ctx context.Context, declared []CapacityExecutable) ([]CapacityExecutable, error) {
	out := make([]CapacityExecutable, 0, len(declared))
	seen := map[string]bool{}
	var total int64
	for _, entry := range declared {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(entry.Path) || len(entry.Path) > 4096 || strings.ContainsRune(entry.Path, 0) {
			return nil, errors.New("capacity: executable path must be absolute and bounded")
		}
		path := filepath.Clean(entry.Path)
		expected, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(expected) != sha256.Size {
			return nil, errors.New("capacity: executable SHA256 must contain 64 hexadecimal characters")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("capacity: resolve executable: %w", err)
		}
		if seen[resolved] {
			return nil, errors.New("capacity: duplicate executable artifact")
		}
		seen[resolved] = true
		observed, size, err := fingerprintCapacityExecutable(ctx, path, capacityInventoryBytes-total)
		if err != nil {
			return nil, err
		}
		total += size
		if observed != hex.EncodeToString(expected) {
			return nil, fmt.Errorf("capacity: executable fingerprint mismatch: %s", path)
		}
		out = append(out, CapacityExecutable{Path: path, SHA256: observed})
	}
	return out, nil
}

func fingerprintCapacityExecutable(ctx context.Context, path string, remaining int64) (string, int64, error) {
	// Stat before open refuses device/FIFO paths without blocking on their open.
	before, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !capacityExecutableMode(runtime.GOOS, path, before.Mode()) {
		return "", 0, errors.New("capacity: declared artifact must be a regular executable file")
	}
	if before.Size() > capacityExecutableBytes || before.Size() > remaining {
		return "", 0, errors.New("capacity: executable inventory byte limit exceeded")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return "", 0, errors.New("capacity: executable changed while opening")
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		n, readErr := f.Read(buf)
		size += int64(n)
		if size > capacityExecutableBytes || size > remaining {
			return "", 0, errors.New("capacity: executable inventory byte limit exceeded")
		}
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(opened, current) || size != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || !current.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("capacity: executable changed during fingerprint verification")
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// Windows has no POSIX executable permission bits. Limit direct native artifact
// declarations to EXE/COM paths; command scripts need an interpreter whose own
// executable must be declared explicitly. This proves artifact identity, while
// the native process launcher remains responsible for binary-format validity.
func capacityExecutableMode(platform, path string, mode os.FileMode) bool {
	if !mode.IsRegular() {
		return false
	}
	if platform == "windows" {
		extension := strings.ToLower(filepath.Ext(path))
		return extension == ".exe" || extension == ".com"
	}
	return mode.Perm()&0111 != 0
}
