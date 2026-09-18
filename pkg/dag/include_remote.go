package dag

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Remote includes let several repositories share one build graph instead of
// each copying the same targets. A repo's dag.md includes a shared file by
// pinned reference; local targets still win name collisions (see mergeFrom),
// so a project overrides only what differs.
//
//	---
//	include:
//	  - gh:qiangli/bashy@v0.19.0/ci.dag.md
//	  - ./local-overrides.dag.md
//	---
//
// The pin is mandatory. An included target's body is executed, so a moving
// reference would mean every dependent repo's build can change without any
// commit in that repo — the same reason promote.yml byte-promotes tested
// artifacts rather than rebuilding. `@ref` may be a tag, branch, or commit
// SHA; a tag or SHA is what makes a build reproducible.
//
// Resolution is offline-first: a pinned ref is immutable by convention, so a
// cached copy is reused without a network call. That is what lets a QA host
// or a CI runner parse the graph with no network, and it is why the cache key
// includes the ref.

// remoteSpec is a parsed `gh:owner/repo@ref/path` include.
type remoteSpec struct {
	Owner string
	Repo  string
	Ref   string
	Path  string
}

// isRemoteInclude reports whether an include entry names a remote source
// rather than a path relative to the including file.
func isRemoteInclude(s string) bool {
	return strings.HasPrefix(s, "gh:") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://")
}

// parseRemoteInclude parses `gh:owner/repo@ref/path/to/file.md`.
//
// Bare http(s) URLs are rejected rather than silently fetched: they carry no
// pin, and failing closed here is the difference between "this build is
// reproducible" and "this build depends on whatever that URL served today".
func parseRemoteInclude(s string) (remoteSpec, error) {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return remoteSpec{}, errf(weavecli.ExitInvalidArg,
			"include %q: bare URLs are not accepted because they cannot be pinned; use gh:owner/repo@ref/path", s)
	}
	rest := strings.TrimPrefix(s, "gh:")
	at := strings.Index(rest, "@")
	if at < 0 {
		return remoteSpec{}, errf(weavecli.ExitInvalidArg,
			"include %q: missing @ref pin; use gh:owner/repo@ref/path (a tag or commit SHA keeps the build reproducible)", s)
	}
	ownerRepo, refPath := rest[:at], rest[at+1:]
	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok || owner == "" || repo == "" {
		return remoteSpec{}, errf(weavecli.ExitInvalidArg, "include %q: expected gh:owner/repo@ref/path", s)
	}
	ref, path, ok := strings.Cut(refPath, "/")
	if !ok || ref == "" || path == "" {
		return remoteSpec{}, errf(weavecli.ExitInvalidArg,
			"include %q: expected gh:owner/repo@ref/path (ref and path are both required)", s)
	}
	return remoteSpec{Owner: owner, Repo: repo, Ref: ref, Path: path}, nil
}

// key identifies a remote include for both cycle detection and the on-disk
// cache. The ref is part of the key: two pins of the same file are two
// different inputs, and caching them together would defeat the pin.
func (r remoteSpec) key() string {
	return fmt.Sprintf("gh:%s/%s@%s/%s", r.Owner, r.Repo, r.Ref, r.Path)
}

func (r remoteSpec) rawURL() string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", r.Owner, r.Repo, r.Ref, r.Path)
}

// cachePath is where a fetched include is stored. Layout mirrors the spec so
// an operator can find (and delete) one by hand.
func (r remoteSpec) cachePath() (string, error) {
	base, err := cacheRoot()
	if err != nil {
		return "", err
	}
	safe := strings.NewReplacer("/", "-", "..", "-").Replace(r.Path)
	return filepath.Join(base, "includes", r.Owner, r.Repo, r.Ref, safe), nil
}

// cacheRoot is the includes cache parent: DAG_CACHE_DIR when set, else the
// user cache dir — the same resolution order cache.go uses for fingerprints,
// so one env var relocates all of dag's state.
func cacheRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("DAG_CACHE_DIR")); dir != "" {
		return dir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", errf(weavecli.ExitInvalidArg, "resolve cache dir: %v", err)
	}
	return filepath.Join(dir, "bashy", "dag"), nil
}

// remoteFetcher retrieves a pinned include. Injectable so tests exercise
// resolution, caching and precedence without network access.
var remoteFetcher = httpFetchInclude

func httpFetchInclude(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// resolveRemoteInclude returns a local path holding the included file,
// fetching it only when it is not already cached. Offline-first: a cache hit
// never touches the network, because the ref is pinned and therefore
// immutable by convention.
func resolveRemoteInclude(spec remoteSpec) (string, error) {
	path, err := spec.cachePath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	body, err := remoteFetcher(spec.rawURL())
	if err != nil {
		return "", errf(weavecli.ExitInvalidArg, "include %s: %v", spec.key(), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", errf(weavecli.ExitInvalidArg, "include %s: cache dir: %v", spec.key(), err)
	}
	// Write via a temp file + rename so a concurrent parse never observes a
	// half-written include.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return "", errf(weavecli.ExitInvalidArg, "include %s: cache write: %v", spec.key(), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", errf(weavecli.ExitInvalidArg, "include %s: cache commit: %v", spec.key(), err)
	}
	return path, nil
}
