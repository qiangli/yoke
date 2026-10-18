package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/ref"
)

// Bounds are per poll, including initialization. Exceeding a bound fails the
// entire batch; it never checkpoints a silently incomplete history.
const RunMaxFiles = 16384
const RunMaxCommits = 256
const RunMaxLogBytes = 1 << 20
const runMaxTokenBytes = 12 << 20

var ErrRunExpired = errors.New("run evidence expired; restart with --since=start")

type runFile struct {
	Size     int64       `json:"size"`
	Modified time.Time   `json:"modified"`
	Mode     fs.FileMode `json:"mode"`
}
type runPosition struct {
	Version  int                `json:"version"`
	Sources  ref.RunSources     `json:"sources"`
	Cutoff   time.Time          `json:"cutoff,omitempty"`
	Head     string             `json:"head"`
	Files    map[string]runFile `json:"files"`
	Offset   int64              `json:"offset"`
	Identity string             `json:"identity"`
	Anchor   string             `json:"anchor"`
}

// RunHistory consumes evidence handles from the existing owning-store resolver.
// File rows are observations (mtime on initial queries, metadata differences on
// resume), commits follow the run's first-parent lineage, and logs are complete
// lines. No status or PID is interpreted as progress.
type RunHistory struct{ Resolver ref.RunSourceResolver }

func (r RunHistory) TranslateTime(ctx context.Context, loc ref.Locator, cutoff time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cutoff.After(time.Now()) {
		return "", fmt.Errorf("run cutoff is in the future")
	}
	return encodeRun(runPosition{Version: 1, Cutoff: cutoff}), nil
}
func encodeRun(p runPosition) string {
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}
func runIdentity(st os.FileInfo) (string, error) {
	v := reflect.ValueOf(st.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		d, i := v.FieldByName("Dev"), v.FieldByName("Ino")
		if d.IsValid() && i.IsValid() {
			return fmt.Sprint(d.Interface(), ":", i.Interface()), nil
		}
	}
	return "", fmt.Errorf("run log identity unavailable")
}
func runAnchor(f *os.File, offset int64) (string, error) {
	b := make([]byte, min(offset, 256))
	_, err := f.ReadAt(b, offset-int64(len(b)))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func (r RunHistory) Delta(ctx context.Context, loc ref.Locator, token string) ([]byte, string, string, error) {
	fail := func(err error) ([]byte, string, string, error) { return nil, "", "", err }
	if loc.Scheme != "run" || loc.ScopeKind != "" || len(loc.Inner) != 0 {
		return fail(ErrUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	sources, err := r.Resolver.RunSources(loc.Target)
	if err != nil {
		return fail(err)
	}
	if sources.Workspace == "" || sources.BaseCommit == "" || sources.StartedAt.IsZero() {
		return fail(fmt.Errorf("run has no persisted workspace/base/start evidence"))
	}
	p := runPosition{Version: 1}
	if token != "" {
		if len(token) > runMaxTokenBytes {
			return fail(fmt.Errorf("run cursor exceeds size bound"))
		}
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(b) > 8<<20 {
			return fail(fmt.Errorf("invalid run cursor"))
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return fail(fmt.Errorf("invalid run cursor: %w", err))
		}
		if _, err := dec.Token(); err != io.EOF || p.Version != 1 || p.Offset < 0 || len(p.Files) > RunMaxFiles {
			return fail(fmt.Errorf("invalid run cursor"))
		}
	}
	resume := p.Head != ""
	if resume && (p.Sources.Workspace != sources.Workspace || p.Sources.BaseCommit != sources.BaseCommit || p.Sources.LogPath != sources.LogPath || !p.Sources.StartedAt.Equal(sources.StartedAt)) {
		return fail(ErrRunExpired)
	}
	now := time.Now().UTC()
	var out bytes.Buffer
	emit := func(kind, path string, attrs map[string]any) {
		b, _ := json.Marshal(Node{SchemaVersion: NodeSchema, Ref: loc.String(), Path: path, Name: filepath.Base(path), Kind: "record", Type: kind, Attrs: attrs})
		out.Write(b)
		out.WriteByte('\n')
	}
	repo, err := ygit.OpenRepository(sources.Workspace)
	if err != nil {
		return fail(fmt.Errorf("%w: workspace unavailable: %v", ErrRunExpired, err))
	}
	head, err := repo.Head()
	if err != nil {
		return fail(err)
	}
	stop := sources.BaseCommit
	if resume {
		stop = p.Head
	}
	hash := head.Hash()
	for count := 0; hash.String() != stop; count++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if count >= RunMaxCommits {
			return fail(fmt.Errorf("run commit history exceeds %d; no cursor advanced", RunMaxCommits))
		}
		c, err := repo.CommitObject(hash)
		if err != nil {
			return fail(fmt.Errorf("%w: commit unavailable", ErrRunExpired))
		}
		if p.Cutoff.IsZero() || (!c.Committer.When.Before(p.Cutoff) && !c.Committer.When.After(now)) {
			emit("commit", hash.String(), map[string]any{"commit": hash.String(), "message": c.Message, "time": c.Committer.When, "evidence": "git-first-parent"})
		}
		if len(c.ParentHashes) == 0 {
			return fail(fmt.Errorf("%w: base/cursor is no longer on first-parent lineage", ErrRunExpired))
		}
		hash = c.ParentHashes[0]
	}
	// Check the stop object too: a missing base must not turn into empty success.
	if _, err := repo.CommitObject(plumbing.NewHash(stop)); err != nil {
		return fail(fmt.Errorf("%w: missing baseline", ErrRunExpired))
	}
	files := map[string]runFile{}
	entries := 0
	err = walkRunFiles(ctx, sources.Workspace, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == sources.Workspace {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > RunMaxFiles {
			return fmt.Errorf("run file scan exceeds %d entries; no cursor advanced", RunMaxFiles)
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sources.Workspace, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f := runFile{st.Size(), st.ModTime(), st.Mode()}
		files[rel] = f
		old, exists := p.Files[rel]
		changed := !resume && (p.Cutoff.IsZero() || (!f.Modified.Before(p.Cutoff) && !f.Modified.After(now))) || resume && (!exists || old.Size != f.Size || old.Mode != f.Mode || !old.Modified.Equal(f.Modified))
		if changed {
			emit("file", rel, map[string]any{"change": "observed", "size": f.Size, "modified": f.Modified, "evidence": "workspace-metadata"})
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	removed := make([]string, 0)
	for path := range p.Files {
		if _, ok := files[path]; !ok {
			removed = append(removed, path)
		}
	}
	sort.Strings(removed)
	for _, path := range removed {
		emit("file", path, map[string]any{"change": "removed", "evidence": "workspace-metadata"})
	}
	if sources.LogPath != "" {
		f, err := os.Open(sources.LogPath)
		if err != nil {
			return fail(fmt.Errorf("%w: log unavailable: %v", ErrRunExpired, err))
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		if !st.Mode().IsRegular() {
			return fail(fmt.Errorf("run log is not regular"))
		}
		identity, err := runIdentity(st)
		if err != nil {
			return fail(err)
		}
		if resume && (identity != p.Identity || st.Size() < p.Offset) {
			return fail(ErrRunExpired)
		}
		if resume {
			a, err := runAnchor(f, p.Offset)
			if err != nil || a != p.Anchor {
				return fail(ErrRunExpired)
			}
		}
		// A time predicate cannot recover timestamps that the writer never stored.
		// An entirely old file can be skipped; an entirely new run is provable.
		skip := !resume && !p.Cutoff.IsZero() && st.ModTime().Before(p.Cutoff)
		if skip {
			// Preserve an incomplete trailing line even when skipping old data.
			tail := make([]byte, min(st.Size(), RunMaxLogBytes))
			if _, err := f.ReadAt(tail, st.Size()-int64(len(tail))); err != nil {
				return fail(err)
			}
			end := bytes.LastIndexByte(tail, '\n')
			if end < 0 && st.Size() > RunMaxLogBytes {
				return fail(fmt.Errorf("run log line exceeds bound"))
			}
			if end >= 0 {
				p.Offset = st.Size() - int64(len(tail)) + int64(end+1)
			}
		} else {
			if st.Size()-p.Offset > RunMaxLogBytes {
				return fail(fmt.Errorf("run log interval exceeds %d bytes; no cursor advanced", RunMaxLogBytes))
			}
			data, err := io.ReadAll(io.NewSectionReader(f, p.Offset, st.Size()-p.Offset))
			if err != nil {
				return fail(err)
			}
			for len(data) > 0 {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					break
				}
				line := data[:i]
				data = data[i+1:]
				start := p.Offset
				p.Offset += int64(i + 1)
				include := true
				if !resume && !p.Cutoff.IsZero() && sources.StartedAt.Before(p.Cutoff) {
					stamp, ok := runLineTime(line)
					if !ok {
						return fail(fmt.Errorf("run log lacks per-line timestamps for requested interval; restart with --since=start"))
					}
					include = !stamp.Before(p.Cutoff) && !stamp.After(now)
				}
				if include {
					emit("log-line", sources.LogPath, map[string]any{"offset": start, "line": string(line), "evidence": "persisted-log"})
				}
			}
		}
		p.Identity = identity
		p.Anchor, err = runAnchor(f, p.Offset)
		if err != nil {
			return fail(err)
		}
	}
	p.Version = 1
	p.Sources = sources
	p.Head = head.Hash().String()
	p.Files = files
	p.Cutoff = time.Time{}
	next := encodeRun(p)
	if len(next) > base64.RawURLEncoding.EncodedLen(8<<20) {
		return fail(fmt.Errorf("run cursor exceeds size bound; no cursor advanced"))
	}
	return out.Bytes(), next, now.Add(24 * time.Hour).Format(time.RFC3339), nil
}

func runLineTime(line []byte) (time.Time, bool) {
	var record map[string]json.RawMessage
	if json.Unmarshal(line, &record) == nil {
		for _, key := range []string{"timestamp", "time", "ts"} {
			var s string
			if json.Unmarshal(record[key], &s) == nil {
				if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
					return t, true
				}
			}
		}
	}
	fields := strings.Fields(string(line))
	if len(fields) > 0 {
		t, e := time.Parse(time.RFC3339Nano, fields[0])
		if e == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Read directories in bounded batches too: WalkDir would first allocate and
// sort every entry in a single directory before the entry cap could fire.
func walkRunFiles(ctx context.Context, root string, visit fs.WalkDirFunc) error {
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > 128 {
			return fmt.Errorf("run file scan exceeds directory depth bound")
		}
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := f.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			for _, d := range entries {
				path := filepath.Join(dir, d.Name())
				if e := visit(path, d, nil); e != nil {
					if e == filepath.SkipDir && d.IsDir() {
						continue
					}
					return e
				}
				if d.IsDir() {
					if e := walk(path, depth+1); e != nil {
						return e
					}
				}
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	return walk(root, 0)
}
