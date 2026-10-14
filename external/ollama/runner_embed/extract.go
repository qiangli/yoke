// Package runner_embed extracts only a runner whose final executable bytes
// match a reviewed release pin and the exact Bashy CI signing identity.
package runner_embed

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const runnerName = "ycode-runner"
const maxRunnerSize int64 = 1 << 30

// Release metadata is committed after the hosted build, audit and signing
// gates pass. An embedded gzip by itself never makes a runner available.
type releaseMetadata struct {
	version, identity, sha256 string
}

// Fill these only with reviewed metadata from an immutable, signed Bashy
// release. Until then the managed service remains unavailable.
const (
	releaseVersion  = "bashy-v0.21.2-15"
	releaseIdentity = "https://github.com/qiangli/ollama/.github/workflows/bashy-build.yml@refs/tags/bashy-v0.21.2-15"
)

var releaseSHA256 = map[string]string{
	"linux/amd64":   "b146dcee3ded0f31c2dfa9e128631e4f7072103b4eada47340cf7a0662a54b59",
	"linux/arm64":   "edfd81603271d2fbf8097e8ceaa455b2c8102061e49b7397b1de57bef2bf14a3",
	"darwin/amd64":  "91caa072772ac2598fad5066c324ff05ca257be4b2dfb4223bf26f27ae9fdc66",
	"darwin/arm64":  "a149f1f2fd27b0df88107b70f6525f08efde09ce25c666372e6a60a71625309b",
	"windows/amd64": "d2a320ae28426e45ec5d77eb3bff84a2b58e2083ec0636e85d9be84362d3858a",
}

var runnerRelease = releaseMetadata{releaseVersion, releaseIdentity, releaseSHA256[runtime.GOOS+"/"+runtime.GOARCH]}
var compressedRunner []byte
var embeddedRunnerBundle []byte
var verifyRunner = binmgr.VerifySignedBlob

func releaseReady() bool {
	r := runnerRelease
	if r.version == "" || r.identity == "" || len(r.sha256) != 64 || len(embeddedRunnerBundle) == 0 {
		return false
	}
	if _, err := hex.DecodeString(r.sha256); err != nil {
		return false
	}
	const workflowPrefix = "https://github.com/qiangli/ollama/.github/workflows/"
	return len(r.identity) > len(workflowPrefix)+len("@refs/tags/")+len(r.version) &&
		strings.HasPrefix(r.identity, workflowPrefix) &&
		strings.HasSuffix(r.identity, "@refs/tags/"+r.version)
}

// Available reports whether an embedded payload and its signed release
// metadata exist. It does not claim the bytes have passed verification.
func Available() bool { return len(compressedRunner) > 0 && releaseReady() }

// EnsureRunner verifies the final executable on every cache hit and after
// extraction. A writable checksum sidecar is never accepted as evidence.
func EnsureRunner(cacheDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("runner: signed Bashy release, reviewed SHA256, or embedded payload is absent")
	}
	name := runnerName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("runner: create cache: %w", err)
	}
	dirInfo, err := os.Lstat(cacheDir)
	if err != nil || !dirInfo.IsDir() || (runtime.GOOS != "windows" && dirInfo.Mode().Perm()&0o022 != 0) {
		return "", fmt.Errorf("runner: cache directory must be a private real directory")
	}
	path := filepath.Join(cacheDir, name)
	verify := func(p string) error {
		return verifyRunner(context.Background(), p, embeddedRunnerBundle, runnerRelease.sha256,
			binmgr.Sigstore{Identity: runnerRelease.identity, Issuer: binmgr.GitHubActionsIssuer})
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("runner: cached executable is not a regular file")
		}
		if (runtime.GOOS == "windows" || info.Mode().Perm()&0o022 == 0) && verify(path) == nil {
			return path, nil
		}
		// Never return an unverified cache entry. The trusted embed may repair it.
	}
	gr, err := gzip.NewReader(bytes.NewReader(compressedRunner))
	if err != nil {
		return "", fmt.Errorf("runner: decompress: %w", err)
	}
	defer gr.Close()
	f, err := os.CreateTemp(cacheDir, ".runner-*")
	if err != nil {
		return "", fmt.Errorf("runner: create temporary executable: %w", err)
	}
	defer os.Remove(f.Name())
	n, copyErr := io.Copy(f, io.LimitReader(gr, maxRunnerSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > maxRunnerSize {
		return "", fmt.Errorf("runner: extraction failed or exceeds size limit: %v %v", copyErr, closeErr)
	}
	if err := verify(f.Name()); err != nil {
		return "", fmt.Errorf("runner: extracted executable verification failed: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o500); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// Windows rename cannot replace an existing tampered cache entry.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("runner: remove invalid cache entry: %w", err)
		}
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", fmt.Errorf("runner: install verified executable: %w", err)
	}
	// Verify the installed path as well, before handing it to a launcher.
	if err := verify(path); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("runner: installed executable verification failed: %w", err)
	}
	return path, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
