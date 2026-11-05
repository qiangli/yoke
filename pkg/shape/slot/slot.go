// Package slot selects immutable, host-local CLI artifacts. It implements
// artifact/pointer mechanics, not verification or authorization policy: callers
// must validate their sealed build evidence before activating a version.
package slot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

const Schema = "bashy-shape-slot-v1"

var ErrNoPrevious = errors.New("shape slot: no previous version")
var ErrUnknownVersion = errors.New("shape slot: unknown version")

// Version names the exact executable bytes retained in a side slot.
type Version struct {
	Revision string    `json:"revision"`
	Digest   string    `json:"digest"`
	Path     string    `json:"path"`
	Evidence string    `json:"evidence,omitempty"`
	Created  time.Time `json:"created"`
}

// State is one atomic host-wide selection; no principal has a separate slot.
type State struct {
	Schema   string   `json:"schema"`
	App      string   `json:"app"`
	Release  *Version `json:"release,omitempty"`
	Active   *Version `json:"active,omitempty"`
	Previous *Version `json:"previous,omitempty"`
}

// Store is independent of principal stores and never replaces a PATH binary.
type Store struct {
	Home, App string
	// ReleaseRevision, when known, records the retained release's source identity.
	ReleaseRevision string
	beforePublish   func(State) error
	afterPublish    func(State) error
}

func (s Store) Dir() (string, error) {
	if s.App == "" || strings.TrimSpace(s.App) != s.App || s.App == "." || s.App == ".." || strings.ContainsAny(s.App, "/\\") {
		return "", errors.New("shape slot: invalid app")
	}
	home := s.Home
	if home == "" {
		home = os.Getenv("BASHY_HOME")
	}
	if home == "" {
		userHome, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		home = filepath.Join(userHome, ".bashy")
	}
	root, e := filepath.Abs(filepath.Join(home, "shape-host", s.App))
	return root, e
}
func (s Store) binaryName() string {
	if runtime.GOOS == "windows" {
		return s.App + ".exe"
	}
	return s.App
}
func validDigest(v string) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == sha256.Size && strings.ToLower(v) == v
}
func (s Store) lock() (*lockfile.Lock, error) {
	root, e := s.Dir()
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	return lockfile.AcquireWithin(filepath.Join(root, ".lock"), 30*time.Second, lockfile.Holder{Name: "shape", Intent: "select host CLI artifact"})
}
func hashFile(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("shape slot: artifact is not a regular file")
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s Store) validateVersion(v Version, checkBytes bool) error {
	if v.Revision == "" || !validDigest(v.Digest) {
		return errors.New("shape slot: invalid artifact identity")
	}
	root, e := s.Dir()
	if e != nil {
		return e
	}
	want := filepath.Join(root, "artifacts", v.Digest, s.binaryName())
	if filepath.Clean(v.Path) != want {
		return errors.New("shape slot: artifact path is outside its immutable slot")
	}
	if !checkBytes {
		return nil
	}
	st, e := os.Lstat(want)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("shape slot: artifact must be a regular file, not a link")
	}
	got, e := hashFile(want)
	if e != nil {
		return e
	}
	if got != v.Digest {
		return errors.New("shape slot: artifact digest changed")
	}
	return nil
}
func versionKey(v Version) string {
	h := sha256.Sum256([]byte(v.Revision + "\x00" + v.Digest))
	return hex.EncodeToString(h[:])
}
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".slot-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	if runtime.GOOS != "windows" {
		dir, e := os.Open(filepath.Dir(path))
		if e != nil {
			return e
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
func (s Store) saveVersion(v Version) error {
	root, e := s.Dir()
	if e != nil {
		return e
	}
	return atomicJSON(filepath.Join(root, "versions", versionKey(v)+".json"), v)
}

// Stage copies bytes into an immutable content-addressed side slot. It never
// changes the active pointer or edits the source executable.
func (s Store) Stage(source, revision, evidence string) (Version, error) {
	l, e := s.lock()
	if e != nil {
		return Version{}, e
	}
	defer l.Release()
	return s.stage(source, revision, evidence)
}
func (s Store) stage(source, revision, evidence string) (Version, error) {
	if strings.TrimSpace(revision) == "" {
		return Version{}, errors.New("shape slot: source revision is required")
	}
	root, e := s.Dir()
	if e != nil {
		return Version{}, e
	}
	in, e := os.Open(source)
	if e != nil {
		return Version{}, e
	}
	defer in.Close()
	st, e := in.Stat()
	if e != nil {
		return Version{}, e
	}
	if !st.Mode().IsRegular() {
		return Version{}, errors.New("shape slot: source artifact must be a regular file")
	}
	temp, e := os.CreateTemp(root, ".artifact-*")
	if e != nil {
		return Version{}, e
	}
	defer os.Remove(temp.Name())
	h := sha256.New()
	_, e = io.Copy(io.MultiWriter(temp, h), in)
	if e == nil {
		mode := os.FileMode(0555)
		if runtime.GOOS == "windows" {
			mode = 0755
		}
		e = temp.Chmod(mode)
	}
	if e == nil {
		e = temp.Sync()
	}
	ce := temp.Close()
	if e != nil {
		return Version{}, e
	}
	if ce != nil {
		return Version{}, ce
	}
	digest := hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(root, "artifacts", digest, s.binaryName())
	if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
		return Version{}, e
	}
	if _, e = os.Lstat(dest); e == nil {
		got, e := hashFile(dest)
		if e != nil {
			return Version{}, e
		}
		if got != digest {
			return Version{}, errors.New("shape slot: existing immutable artifact was modified")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return Version{}, e
	} else if e = os.Rename(temp.Name(), dest); e != nil {
		return Version{}, e
	}
	v := Version{Revision: revision, Digest: digest, Path: dest, Evidence: evidence, Created: time.Now().UTC()}
	if e = s.validateVersion(v, true); e != nil {
		return Version{}, e
	}
	if e = s.saveVersion(v); e != nil {
		return Version{}, e
	}
	return v, nil
}
func (s Store) read() (State, error) {
	root, e := s.Dir()
	if e != nil {
		return State{}, e
	}
	st := State{Schema: Schema, App: s.App}
	b, e := os.ReadFile(filepath.Join(root, "state.json"))
	if errors.Is(e, os.ErrNotExist) {
		return st, nil
	}
	if e != nil {
		return st, e
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&st); e != nil {
		return st, e
	}
	if st.Schema != Schema || st.App != s.App {
		return st, errors.New("shape slot: state identity mismatch")
	}
	for _, v := range []*Version{st.Release, st.Active, st.Previous} {
		if v != nil {
			if e = s.validateVersion(*v, false); e != nil {
				return st, e
			}
		}
	}
	return st, nil
}
func (s Store) publish(st State) (State, error) {
	if s.beforePublish != nil {
		if e := s.beforePublish(st); e != nil {
			return st, e
		}
	}
	root, e := s.Dir()
	if e != nil {
		return st, e
	}
	if e = atomicJSON(filepath.Join(root, "state.json"), st); e != nil {
		return st, e
	}
	if s.afterPublish != nil {
		if e = s.afterPublish(st); e != nil {
			return st, e
		}
	}
	return st, nil
}

// Status reports the retained identities and refuses an invalid active artifact.
func (s Store) Status() (State, error) {
	st, e := s.read()
	if e != nil {
		return st, e
	}
	if st.Active != nil {
		e = s.validateVersion(*st.Active, true)
	}
	return st, e
}

// Select returns only a verified-by-digest executable from this store. No active
// selection is a normal false result, leaving the installed release in use.
func (s Store) Select() (Version, bool, error) {
	st, e := s.Status()
	if e != nil {
		return Version{}, false, e
	}
	if st.Active == nil {
		return Version{}, false, nil
	}
	return *st.Active, true, nil
}

// Activate retains the original released bytes before atomically selecting v.
// The installed release and PATH are never changed. Verification policy belongs
// to the caller; this function rechecks the staged executable's digest.
func (s Store) Activate(v Version, releasePath string) (State, error) {
	l, e := s.lock()
	if e != nil {
		return State{}, e
	}
	defer l.Release()
	if e = s.validateVersion(v, true); e != nil {
		return State{}, e
	}
	st, e := s.read()
	if e != nil {
		return st, e
	}
	if st.Release == nil {
		if releasePath == "" {
			return st, errors.New("shape slot: retained release executable is required")
		}
		revision := s.ReleaseRevision
		if revision == "" {
			revision = "unknown"
		}
		release, e := s.stage(releasePath, revision, "")
		if e != nil {
			return st, fmt.Errorf("shape slot: retain release: %w", e)
		}
		st.Release = &release
	}
	if st.Active != nil && st.Active.Digest == v.Digest && st.Active.Revision == v.Revision {
		return st, nil
	}
	if e = s.saveVersion(v); e != nil {
		return st, e
	}
	st.Previous = st.Active
	if st.Previous == nil {
		st.Previous = st.Release
	}
	st.Active = &v
	return s.publish(st)
}

// Undo swaps to the preceding retained version. A corrupt current executable
// does not prevent recovery, because only the target's bytes are executed.
func (s Store) Undo() (State, error) {
	l, e := s.lock()
	if e != nil {
		return State{}, e
	}
	defer l.Release()
	st, e := s.read()
	if e != nil {
		return st, e
	}
	if st.Previous == nil {
		return st, ErrNoPrevious
	}
	if e = s.validateVersion(*st.Previous, true); e != nil {
		return st, e
	}
	st.Active, st.Previous = st.Previous, st.Active
	return s.publish(st)
}

// Restore selects the retained release, an exact artifact digest, or an
// unambiguous retained source revision. It never rebuilds or fetches code.
func (s Store) Restore(ref string) (State, error) {
	l, e := s.lock()
	if e != nil {
		return State{}, e
	}
	defer l.Release()
	st, e := s.read()
	if e != nil {
		return st, e
	}
	var selected *Version
	if ref == "release" || ref == "base" {
		selected = st.Release
	} else {
		root, _ := s.Dir()
		entries, e := os.ReadDir(filepath.Join(root, "versions"))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return st, e
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			b, e := os.ReadFile(filepath.Join(root, "versions", entry.Name()))
			if e != nil {
				return st, e
			}
			var v Version
			if e = json.Unmarshal(b, &v); e != nil {
				return st, e
			}
			if v.Revision != ref && v.Digest != ref {
				continue
			}
			if e = s.validateVersion(v, true); e != nil {
				return st, e
			}
			if selected != nil && selected.Digest != v.Digest {
				return st, errors.New("shape slot: revision has multiple artifacts; restore an exact digest")
			}
			copy := v
			selected = &copy
		}
	}
	if selected == nil {
		return st, ErrUnknownVersion
	}
	if e = s.validateVersion(*selected, true); e != nil {
		return st, e
	}
	if st.Active != nil && st.Active.Digest == selected.Digest && st.Active.Revision == selected.Revision {
		return st, nil
	}
	st.Previous = st.Active
	st.Active = selected
	return s.publish(st)
}

// RetainedRelease validates the recovery image independently of the current
// artifact's health, so a damaged shaped executable does not hide recovery.
func (s Store) RetainedRelease() (Version, bool, error) {
	st, e := s.read()
	if e != nil {
		return Version{}, false, e
	}
	if st.Release == nil {
		return Version{}, false, nil
	}
	if e = s.validateVersion(*st.Release, true); e != nil {
		return Version{}, false, e
	}
	return *st.Release, true, nil
}
