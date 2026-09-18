// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package reduce implements the shared command-output-reduction pipeline. Its
// conservative Stage 0.1 removes only exact repeats from a closed telemetry
// hint registry; Stage A1 writes the complete pre-reduction artifact BEFORE any
// reduced view is emitted and carries a RUNNABLE recovery command inline. This
// turns every reduction from lossy into lossy-in-context / lossless-in-system.
//
// This package owns no store LOCATION. Like pkg/admission it is a primitive: the
// embedding host points Store at its existing command/session/run artifact path
// so there is no seventh store, and wires a Redactor so redaction precedes spill.
package reduce

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	dirMode  = 0o700
	fileMode = 0o600

	// digestHexLen is the length of a lowercase hex sha256.
	digestHexLen = sha256.Size * 2

	// MinHandleLen is the shortest digest-prefix handle the marker will show.
	// The handle is content-addressed rather than a counter (a counter is a
	// read-modify-write that needs a lock and breaks byte-identical markers
	// across two identical runs), so it is safe to shorten to a readable prefix
	// and lengthen only when a collision is actually observed.
	MinHandleLen = 8
)

// digestPrefix is the scheme label carried by a full digest string.
const digestPrefix = "sha256:"

// Redactor masks secret material. secrets.Redactor satisfies it. It is applied
// to the complete bytes BEFORE they are spilled, so the authoritative artifact —
// the one the handle makes retrievable later — inherits the same secret gate as
// any other capture path (contract §2.7).
type Redactor interface {
	Redact([]byte) []byte
}

// Store is a content-addressed blob directory rooted at an existing artifact
// path. On Unix, blobs are kept at mode 0600. Put writes a temp file and uses
// an atomic replace into the digest name; a content address lets racing
// identical writers converge without coordination.
type Store struct {
	root string
}

// NewStore roots a spill store at dir. It creates nothing until the first Put,
// so a session that never spills pays no IO. dir is expected to be a
// subdirectory of the host's existing command/session/run artifact path.
func NewStore(dir string) *Store { return &Store{root: strings.TrimSpace(dir)} }

// Root reports the directory this store writes under.
func (s *Store) Root() string { return s.root }

// Put writes content to a content-addressed blob and returns its full digest
// ("sha256:<hex>"). It is idempotent: identical content maps to one filename, so
// a repeat Put is a no-op create that still reports the digest. The bytes are
// written to a temp file and atomically renamed into place so a reader never
// observes a partially written blob.
func (s *Store) Put(content []byte) (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("reduce: spill store has no root")
	}
	sum := sha256.Sum256(content)
	hexsum := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(s.root, dirMode); err != nil {
		return "", err
	}
	final := filepath.Join(s.root, hexsum)
	if _, err := os.Stat(final); err == nil {
		if err := ensureBlobMode(final); err != nil {
			return "", err
		}
		return digestPrefix + hexsum, nil
	}
	tmp, err := os.CreateTemp(s.root, hexsum+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		// A concurrent writer of the same content may have won the race; the
		// content address guarantees the destination is byte-identical either way.
		if _, statErr := os.Stat(final); statErr == nil {
			os.Remove(tmpName)
			if modeErr := ensureBlobMode(final); modeErr != nil {
				return "", modeErr
			}
			return digestPrefix + hexsum, nil
		}
		os.Remove(tmpName)
		return "", err
	}
	return digestPrefix + hexsum, nil
}

func ensureBlobMode(name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(name, fileMode); err != nil {
		return fmt.Errorf("reduce: enforce spill permissions: %w", err)
	}
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != fileMode {
		return fmt.Errorf("reduce: spill blob mode = %o, want %o", info.Mode().Perm(), fileMode)
	}
	return nil
}

// Get resolves a digest-prefix handle to its exact stored bytes. The handle may
// be a bare hex prefix ("9c2d4f1a"), a full hex digest, or a "sha256:"-prefixed
// digest. An ambiguous prefix is an error naming the collision so the caller can
// lengthen it; a prefix that matches nothing is a distinct error.
func (s *Store) Get(handle string) ([]byte, string, error) {
	name, err := s.resolve(handle)
	if err != nil {
		return nil, "", err
	}
	content, err := os.ReadFile(filepath.Join(s.root, name))
	if err != nil {
		return nil, "", err
	}
	return content, digestPrefix + name, nil
}

// resolve maps a handle to the single blob filename it identifies.
func (s *Store) resolve(handle string) (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("reduce: spill store has no root")
	}
	want := normalizeHandle(handle)
	if want == "" {
		return "", errors.New("reduce: empty recovery handle")
	}
	if !isHex(want) {
		return "", fmt.Errorf("reduce: recovery handle %q is not a hex digest prefix", handle)
	}
	if len(want) == digestHexLen {
		if _, err := os.Stat(filepath.Join(s.root, want)); err != nil {
			return "", fmt.Errorf("reduce: no spilled output for %s", handle)
		}
		return want, nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return "", fmt.Errorf("reduce: no spilled output for %s", handle)
	}
	var matches []string
	for _, e := range entries {
		n := e.Name()
		if len(n) == digestHexLen && strings.HasPrefix(n, want) {
			matches = append(matches, n)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("reduce: no spilled output for %s", handle)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("reduce: recovery handle %s is ambiguous across %d blobs; use a longer prefix (e.g. %s)",
			handle, len(matches), matches[0][:min(len(matches[0]), len(want)+MinHandleLen)])
	}
}

// shortestHandle returns the shortest prefix of hexsum, no shorter than
// MinHandleLen, that no other blob in the store shares. It is the handle the
// marker carries. On any read error it falls back to a MinHandleLen prefix
// rather than failing the reduction — the recovery command degrades to
// requiring a longer prefix, which is strictly better than emitting no marker.
func (s *Store) shortestHandle(hexsum string) string {
	n := min(MinHandleLen, len(hexsum))
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return hexsum[:n]
	}
	for {
		clash := false
		for _, e := range entries {
			name := e.Name()
			if len(name) != digestHexLen || name == hexsum {
				continue
			}
			if strings.HasPrefix(name, hexsum[:n]) {
				clash = true
				break
			}
		}
		if !clash || n >= len(hexsum) {
			return hexsum[:n]
		}
		n++
	}
}

func normalizeHandle(handle string) string {
	h := strings.TrimSpace(handle)
	h = strings.TrimPrefix(h, digestPrefix)
	return strings.ToLower(h)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
