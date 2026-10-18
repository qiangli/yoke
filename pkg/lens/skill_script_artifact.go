package lens

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ScriptArtifactSuffix is the file extension used for content-addressed script artifacts.
const ScriptArtifactSuffix = ".sh"

// ScriptArtifactName computes the deterministic, content-addressed artifact filename
// for already-compiled script bytes. The artifact name is the lowercase hex sha256
// digest of the script bytes followed by ".sh".
func ScriptArtifactName(script []byte) string {
	sum := sha256.Sum256(script)
	return hex.EncodeToString(sum[:]) + ScriptArtifactSuffix
}

// ScriptArtifactDigest computes the canonical sha256 digest string ("sha256:<hex>")
// for the given script bytes.
func ScriptArtifactDigest(script []byte) string {
	sum := sha256.Sum256(script)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ScriptArtifactPath returns the full filesystem path of the script artifact under dir.
func ScriptArtifactPath(dir string, script []byte) string {
	return filepath.Join(dir, ScriptArtifactName(script))
}

// StoreScriptArtifact turns already-compiled script bytes into a content-addressed
// artifact written atomically under caller-provided dir (Sprint 446 S22). It reuses
// matching bytes if the artifact already exists, rejects a different preexisting
// object or symlink fail-closed with ExitInvalid, and returns the artifact filename
// and its sha256 digest ("sha256:<hex>").
//
// It never executes the script, infers no effects, and makes no model calls.
func StoreScriptArtifact(dir string, script []byte) (name string, digest string, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", "", errorf(ExitUsage, "artifact store directory is required")
	}

	name = ScriptArtifactName(script)
	digest = ScriptArtifactDigest(script)
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if info, err := os.Lstat(dir); err != nil {
		return "", "", err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errorf(ExitInvalid, "script artifact directory is not a real directory")
	}

	// Check whether an object already exists at the target path.
	// os.Lstat is required so symlinks are never followed.
	if fi, statErr := os.Lstat(target); statErr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", "", errorf(ExitInvalid, "script artifact %s is a symlink", name)
		}
		if !fi.Mode().IsRegular() {
			return "", "", errorf(ExitInvalid, "script artifact %s is not a regular file", name)
		}
		existing, readErr := os.ReadFile(target)
		if readErr != nil {
			return "", "", readErr
		}
		if !bytes.Equal(existing, script) {
			return "", "", errorf(ExitInvalid, "script artifact %s exists with differing content", name)
		}
		// Existing file matches byte-for-byte; reuse it without rewriting.
		return name, digest, nil
	} else if !errors.Is(statErr, os.ErrNotExist) && !os.IsNotExist(statErr) {
		return "", "", statErr
	}

	// Write a temporary file in the same directory, then publish it without
	// replacing any existing object.
	tmp, err := os.CreateTemp(dir, ".tmp-script-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", "", err
	}
	if _, err := tmp.Write(script); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}

	// Link creates the final name atomically without replacing an object that
	// appeared after the first Lstat. Rename would overwrite that object.
	if err := os.Link(tmpName, target); err != nil {
		// A concurrent writer of the same content may have won the race.
		if fi, statErr := os.Lstat(target); statErr == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return "", "", errorf(ExitInvalid, "script artifact %s is a symlink", name)
			}
			if !fi.Mode().IsRegular() {
				return "", "", errorf(ExitInvalid, "script artifact %s is not a regular file", name)
			}
			existing, readErr := os.ReadFile(target)
			if readErr == nil && bytes.Equal(existing, script) {
				return name, digest, nil
			}
			return "", "", errorf(ExitInvalid, "script artifact %s exists with differing content", name)
		}
		return "", "", err
	}

	return name, digest, nil
}

// ScriptArtifact is an alias for StoreScriptArtifact.
func ScriptArtifact(dir string, script []byte) (name string, digest string, err error) {
	return StoreScriptArtifact(dir, script)
}

// WriteScriptArtifact is an alias for StoreScriptArtifact.
func WriteScriptArtifact(dir string, script []byte) (name string, digest string, err error) {
	return StoreScriptArtifact(dir, script)
}
