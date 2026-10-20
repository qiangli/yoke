package dagrun

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// homeKeyPrefix is the readable prefix of every workspace home key; the rest
// of the key is the vknode volume-name hash, so an operator reading `podman
// volume ls` or a host directory listing can tell a home from a volume only
// by its owner, never by opaque hex.
const homeKeyPrefix = "bashy-home-"

// HomeKey is the deterministic, label/path-safe key of a workspace home. It
// mirrors outpost's vknode hostPathVolumeName scheme (sha1 of the NUL-joined
// clusterID, namespace and workspaceID, first 8 bytes as hex) so the two
// spellings stay comparable across the executor boundary; an empty clusterID
// omits its segment, keeping the legacy unscoped names byte-identical.
func HomeKey(clusterID, namespace, workspaceID string) string {
	key := namespace + "\x00" + workspaceID
	if clusterID != "" {
		key = clusterID + "\x00" + key
	}
	sum := sha1.Sum([]byte(key))
	return homeKeyPrefix + hex.EncodeToString(sum[:8])
}

// HomePath is the HostPath a Linux executor mounts as the workspace home
// (render.go's `home` volume). The renderer can adopt this helper later; the
// two must never drift.
func HomePath(workspaceID string) string {
	return HomeHostRoot + "/" + workspaceID
}

var (
	// ErrInvalidHome is returned by Affinity for a missing home record or
	// an unnamed executor candidate. An empty candidate never binds.
	ErrInvalidHome = errors.New("dagrun: invalid home record")
	// ErrHomeElsewhere is returned when a workspace home already lives on
	// a different executor. Moving a home is out of v1 scope.
	ErrHomeElsewhere = errors.New("dagrun: home lives on another executor")
)

// HomeRecord names one workspace home and the executor it lives on. Key and
// CreatedAt are caller-supplied, like Session: this package keeps no clock
// and no cluster identity of its own.
type HomeRecord struct {
	WorkspaceID  string    `json:"workspace_id"`
	ExecutorHost string    `json:"executor_host,omitempty"`
	Key          string    `json:"key"`
	CreatedAt    time.Time `json:"created_at"`
}

// Affinity places a home on an executor: the first placement binds the home
// to candidate (rec.ExecutorHost is set), later placements on the same
// executor succeed, and any other executor fails with ErrHomeElsewhere
// naming the bound host. A home never moves in v1.
func Affinity(rec *HomeRecord, candidate string) error {
	if rec == nil {
		return fmt.Errorf("%w: no home record", ErrInvalidHome)
	}
	if candidate == "" {
		return fmt.Errorf("%w: no executor candidate", ErrInvalidHome)
	}
	if rec.ExecutorHost == "" {
		rec.ExecutorHost = candidate
		return nil
	}
	if rec.ExecutorHost != candidate {
		return fmt.Errorf("%w: home %s for workspace %q lives on %q, not %q",
			ErrHomeElsewhere, rec.Key, rec.WorkspaceID, rec.ExecutorHost, candidate)
	}
	return nil
}

var (
	// ErrInvalidClaim is returned when a claim or release names no
	// workspace, owner or repo, or the path is outside the home.
	ErrInvalidClaim = errors.New("dagrun: invalid working-copy claim")
	// ErrOwnershipConflict is returned when a working copy already belongs
	// to a different owner, or a non-owner tries to release it.
	ErrOwnershipConflict = errors.New("dagrun: working copy owned by another owner")
	// ErrStorageUnavailable is returned when the home directory is missing
	// or unwritable. Nothing is ever written elsewhere.
	ErrStorageUnavailable = errors.New("dagrun: workspace storage unavailable")
)

// workingCopiesFile is the ownership registry beside the working copies.
const workingCopiesFile = "working-copies.json"

// workingCopyEntry is one registered working copy: its clone-directory name
// keyed by owner and workspace.
type workingCopyEntry struct {
	Owner     string `json:"owner"`
	Workspace string `json:"workspace"`
}

// workingCopyRegistry is the JSON document persisted in the home directory.
type workingCopyRegistry struct {
	Copies map[string]workingCopyEntry `json:"copies"`
}

// WorkingCopies hands out the isolated per-owner working copies of one
// workspace home: weave clones into the claimed directories later, so this
// type only owns paths and their ownership records — no git, no network, no
// tree management. It is safe for concurrent use.
type WorkingCopies struct {
	mu  sync.Mutex
	dir string
}

// NewWorkingCopies returns the working-copy registry over dir, the
// caller-given workspace home directory. dir itself is not touched until a
// Claim or Release needs it.
func NewWorkingCopies(dir string) *WorkingCopies {
	return &WorkingCopies{dir: dir}
}

// Claim returns ownerID's working copy of repo inside the home: a distinct
// per-owner directory named weave-clone style, <repo>-<owner>. The first
// claim records ownership in the registry (atomic temp+rename write);
// re-claiming an own path is idempotent, while a claim folding onto another
// owner's path fails with ErrOwnershipConflict. A missing or unwritable home
// fails with ErrStorageUnavailable and writes nothing anywhere.
func (w *WorkingCopies) Claim(workspaceID, ownerID, repo string) (string, error) {
	if workspaceID == "" || ownerID == "" || repo == "" {
		return "", fmt.Errorf("%w: workspace, owner and repo are required", ErrInvalidClaim)
	}
	name := workingCopyName(repo, ownerID)
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "", fmt.Errorf("%w: repo %q and owner %q fold to %q", ErrInvalidClaim, repo, ownerID, name)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	reg, err := w.load()
	if err != nil {
		return "", err
	}
	if prev, ok := reg.Copies[name]; ok && prev.Owner != ownerID {
		return "", fmt.Errorf("%w: %s belongs to %q",
			ErrOwnershipConflict, filepath.Join(w.dir, name), prev.Owner)
	}
	reg.Copies[name] = workingCopyEntry{Owner: ownerID, Workspace: workspaceID}
	if err := w.save(reg); err != nil {
		return "", err
	}
	return filepath.Join(w.dir, name), nil
}

// Release drops ownerID's ownership of path (a path Claim returned). The
// directory tree is not touched — weave owns the clones — but the name is
// free for the next owner to claim. Releasing another owner's path fails
// with ErrOwnershipConflict; an unregistered path is an error.
func (w *WorkingCopies) Release(ownerID, path string) error {
	if ownerID == "" || path == "" {
		return fmt.Errorf("%w: owner and path are required", ErrInvalidClaim)
	}
	name, err := filepath.Rel(w.dir, path)
	if err != nil || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is not inside the home %q", ErrInvalidClaim, path, w.dir)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	reg, err := w.load()
	if err != nil {
		return err
	}
	prev, ok := reg.Copies[name]
	if !ok {
		return fmt.Errorf("%w: %q is not a registered working copy", ErrInvalidClaim, path)
	}
	if prev.Owner != ownerID {
		return fmt.Errorf("%w: %s belongs to %q", ErrOwnershipConflict, path, prev.Owner)
	}
	delete(reg.Copies, name)
	return w.save(reg)
}

// load reads the registry from the home directory, requiring the home to
// exist. A missing registry file is an empty registry.
func (w *WorkingCopies) load() (*workingCopyRegistry, error) {
	fi, err := os.Stat(w.dir)
	if err != nil {
		return nil, fmt.Errorf("%w: home %q: %v", ErrStorageUnavailable, w.dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q is not a directory", ErrStorageUnavailable, w.dir)
	}
	reg := &workingCopyRegistry{Copies: map[string]workingCopyEntry{}}
	raw, err := os.ReadFile(filepath.Join(w.dir, workingCopiesFile))
	if err == nil {
		if err := json.Unmarshal(raw, reg); err != nil {
			return nil, fmt.Errorf("dagrun: decode working-copy registry: %w", err)
		}
		if reg.Copies == nil {
			reg.Copies = map[string]workingCopyEntry{}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("dagrun: read working-copy registry: %w", err)
	}
	return reg, nil
}

// save persists the registry with an atomic temp+rename write, so a crash
// never leaves a half-written registry behind.
func (w *WorkingCopies) save(reg *workingCopyRegistry) error {
	raw, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("dagrun: encode working-copy registry: %w", err)
	}
	tmp, err := os.CreateTemp(w.dir, workingCopiesFile+".tmp*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("dagrun: write working-copy registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("dagrun: close working-copy registry: %w", err)
	}
	if err := os.Rename(name, filepath.Join(w.dir, workingCopiesFile)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	return nil
}

// workingCopyName is the weave-clone style directory name, <repo>-<owner>,
// with each segment folded into one path-safe directory component. The
// registry, not the name, decides ownership when two claims fold alike.
func workingCopyName(repo, ownerID string) string {
	seg := func(s string) string {
		out := make([]byte, 0, len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
				c == '-', c == '_', c == '.':
				out = append(out, c)
			default:
				out = append(out, '_')
			}
		}
		return string(out)
	}
	return seg(repo) + "-" + seg(ownerID)
}
