package feed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// IndexName is the per-sink file listing published posts, one relPath
	// per line. It is how a follower discovers posts without directory
	// listing support on every transport.
	IndexName = "index.txt"

	maxPostBytes   = 4 << 20
	maxIndexLines  = 10000
	followsFile    = "follows.json"
	httpFetchLimit = 30 * time.Second
)

func followsPath(dir string) string { return filepath.Join(dir, followsFile) }

// LoadFollows reads dir/follows.json (missing = none).
func LoadFollows(dir string) ([]string, error) {
	b, err := os.ReadFile(followsPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed: load follows: %w", err)
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("feed: load follows: %w", err)
	}
	return out, nil
}

// SaveFollows writes the sorted, de-duplicated list atomically.
func SaveFollows(dir string, refs []string) error {
	seen := map[string]bool{}
	list := make([]string, 0, len(refs))
	for _, r := range refs {
		if r != "" && !seen[r] {
			seen[r] = true
			list = append(list, r)
		}
	}
	sort.Strings(list)
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("feed: save follows: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "follows-*.json")
	if err != nil {
		return fmt.Errorf("feed: save follows: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("feed: save follows: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("feed: save follows: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return fmt.Errorf("feed: save follows: %w", err)
	}
	if err := os.Rename(name, followsPath(dir)); err != nil {
		os.Remove(name)
		return fmt.Errorf("feed: save follows: %w", err)
	}
	return nil
}

// NormalizeFollow canonicalizes a follow reference: http(s) URLs lose any
// trailing slash; an existing local directory becomes an absolute path;
// anything else containing ':' is an rclone target ("remote:path").
func NormalizeFollow(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("feed: follow reference is empty")
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return strings.TrimRight(ref, "/"), nil
	}
	if st, err := os.Stat(ref); err == nil && st.IsDir() {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	if strings.Contains(ref, ":") {
		return strings.TrimRight(ref, "/"), nil
	}
	return "", fmt.Errorf("feed: %q is not an https URL, an rclone target (remote:path), or an existing directory", ref)
}

// Source reads posts from one followed sink base.
type Source interface {
	Get(ctx context.Context, rel string) ([]byte, error)
}

// OpenSource builds the reader for a follow reference. client is used for
// http(s) bases (nil = a default client).
func OpenSource(ref string, client *http.Client) (Source, error) {
	switch {
	case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
		if client == nil {
			client = &http.Client{Timeout: httpFetchLimit}
		}
		return &httpSource{base: strings.TrimRight(ref, "/"), client: client}, nil
	case isDir(ref):
		return &dirSource{root: ref}, nil
	case strings.Contains(ref, ":"):
		return &rcloneSource{base: strings.TrimRight(ref, "/")}, nil
	}
	return nil, fmt.Errorf("feed: cannot read follow %q", ref)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

type httpSource struct {
	base   string
	client *http.Client
}

func (s *httpSource) Get(ctx context.Context, rel string) ([]byte, error) {
	rp, err := validateRelPath(rel)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+rp, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feed: GET %s/%s: %w", s.base, rp, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed: GET %s/%s: %s", s.base, rp, resp.Status)
	}
	return readCapped(resp.Body)
}

type dirSource struct{ root string }

func (s *dirSource) Get(_ context.Context, rel string) ([]byte, error) {
	rp, err := validateRelPath(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(rp)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCapped(f)
}

type rcloneSource struct{ base string }

func (s *rcloneSource) Get(ctx context.Context, rel string) ([]byte, error) {
	rp, err := validateRelPath(rel)
	if err != nil {
		return nil, err
	}
	return runRclone(ctx, "cat", s.base+"/"+rp)
}

func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxPostBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPostBytes {
		return nil, fmt.Errorf("feed: remote file exceeds %d bytes", maxPostBytes)
	}
	return b, nil
}

// ParseIndex returns the relPaths in an index file. Index content comes from
// a remote party, so unsafe or oversized entries are dropped and reported in
// skipped rather than trusted.
func ParseIndex(data []byte) (rels []string, skipped []string) {
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if len(rels) >= maxIndexLines {
			skipped = append(skipped, "index truncated at "+fmt.Sprint(maxIndexLines)+" entries")
			break
		}
		rp, err := validateRelPath(line)
		if err != nil || rp == IndexName {
			skipped = append(skipped, line)
			continue
		}
		if !seen[rp] {
			seen[rp] = true
			rels = append(rels, rp)
		}
	}
	return rels, skipped
}

// appendIndex adds rel to a sink's index.txt by read-modify-write through
// Sink.Get/Put. A missing index starts a new one; any other read error
// aborts rather than clobbering an index we could not read.
func appendIndex(ctx context.Context, s Sink, rel string) error {
	cur, err := s.Get(ctx, IndexName)
	if err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("feed: reading %s on sink %q: %w", IndexName, s.Name(), err)
		}
		cur = nil
	}
	for _, l := range strings.Split(string(cur), "\n") {
		if strings.TrimSpace(l) == rel {
			return nil
		}
	}
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		cur = append(cur, '\n')
	}
	cur = append(cur, rel+"\n"...)
	if _, err := s.Put(ctx, IndexName, cur); err != nil {
		return fmt.Errorf("feed: writing %s on sink %q: %w", IndexName, s.Name(), err)
	}
	return nil
}

func isNotFound(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"not found", "no such file", "does not exist", "doesn't exist"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
