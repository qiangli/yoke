// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package binmgr

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ManagedSpec describes a managed component (an engine or tool) and how to make
// it available. ProvisionManaged resolves it with ONE process shared by every
// consumer — bashy, outpost, and Tessaro apps — so podman/ollama/<new X> are
// provisioned identically everywhere, and always **standalone**: a paired host is
// never assumed.
type ManagedSpec struct {
	// Name is the cache key and the resulting executable's basename.
	Name string
	// DestDir is where the executable is installed (a stable path a fast cache
	// check can find without network). "" = binmgr's CacheDir().
	DestDir string
	// ReleaseRepo is "owner/repo" whose latest release carries a prebuilt blob
	// named <name>-<goos>-<goarch>.gz (built from permissive source in CI). "" =
	// no prebuilt step.
	ReleaseRepo string
	// Deps are provisioned first, into the same DestDir (e.g. podman → vfkit,
	// gvproxy on macOS). Best-effort.
	Deps []ManagedSpec
	// Build builds the component from source into dest when no prebuilt blob is
	// available (the licensing-policy fallback: permissive source + toolchain).
	// nil = fetch-only.
	Build func(ctx context.Context, dest string) error
	// Log receives progress lines. Optional.
	Log func(string)
}

func (s ManagedSpec) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, a...))
	}
}

// ProvisionManaged resolves spec to a cached executable path. Resolution order —
// the single shared pipeline:
//
//  1. cache hit at DestDir/<name>                         → return (no network)
//  2. fetch <name>-<goos>-<goarch>.gz from ReleaseRepo    → gunzip → DestDir/<name>
//  3. Build from source (if provided)                     → DestDir/<name>
//  4. error
//
// Deps are provisioned first (into the same dir). Standalone throughout — nothing
// here needs a paired host; a caller layers mesh delegation ON TOP if it wants.
func ProvisionManaged(ctx context.Context, spec ManagedSpec) (string, error) {
	dir := spec.DestDir
	if dir == "" {
		d, err := CacheDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	dest := filepath.Join(dir, binaryName(spec.Name))
	if isExecFile(dest) {
		return dest, nil // 1. cache hit
	}
	for _, dep := range spec.Deps {
		if dep.DestDir == "" {
			dep.DestDir = dir
		}
		if dep.Log == nil {
			dep.Log = spec.Log
		}
		_, _ = ProvisionManaged(ctx, dep) // best-effort (e.g. the podman VM helpers)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if spec.ReleaseRepo != "" { // 2. prebuilt blob
		if err := fetchReleaseGz(ctx, spec.ReleaseRepo, spec.Name, dest, spec.logf); err == nil && isExecFile(dest) {
			return dest, nil
		} else if err != nil {
			spec.logf("binmgr: %s: no prebuilt blob (%v)", spec.Name, err)
		}
	}
	if spec.Build != nil { // 3. build from permissive source
		spec.logf("binmgr: %s: building from source…", spec.Name)
		if err := spec.Build(ctx, dest); err == nil && isExecFile(dest) {
			return dest, nil
		} else if err != nil {
			spec.logf("binmgr: %s: build failed (%v)", spec.Name, err)
		}
	}
	return "", fmt.Errorf("binmgr: cannot provision %s (no cache, no prebuilt blob, no source build)", spec.Name)
}

// fetchReleaseGz downloads <name>-<goos>-<goarch>.gz from repo's latest release,
// gunzips it into dest atomically, and marks it executable.
func fetchReleaseGz(ctx context.Context, repo, name, dest string, logf func(string, ...any)) error {
	rel, err := fetchRelease(ctx, repo, "")
	if err != nil {
		return err
	}
	goos, goarch := splitPlatform(Platform())
	want := fmt.Sprintf("%s-%s-%s.gz", name, goos, goarch)
	var url string
	for _, a := range rel.Assets {
		if a.Name == want {
			url = a.URL
			break
		}
	}
	if url == "" {
		return fmt.Errorf("no asset %s in %s@%s", want, repo, rel.TagName)
	}
	logf("binmgr: fetching %s (%s, %s)", name, want, rel.TagName)
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()
	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, gz); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func isExecFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
