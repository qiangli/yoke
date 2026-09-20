package weave

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type SessionPointer struct {
	TaskID       string `json:"task_id"`
	SprintID     string `json:"sprint_id,omitempty"`
	CloudboxBase string `json:"cloudbox_base"`
	TokenRef     string `json:"token_ref"`
	// RepoKey is the normalized origin (`github.com/org/repo`) the session
	// was derived from — recorded so a later reader can tell WHY this task
	// and detect a remote that moved.
	RepoKey string `json:"repo_key,omitempty"`
	// SprintSeq is the manager's sprint number bound to this session, so a
	// commit on another host may carry `Sprint: #<seq>` for a sprint that
	// exists on the manager's board only.
	SprintSeq int64 `json:"sprint_seq,omitempty"`
	// Role is the seat cloudbox gave this account on the session at the
	// last join: owner · contributor · observer. An observer can read the
	// board and send/receive mail; it cannot steer or take the lease.
	Role string `json:"role,omitempty"`
}

func ReadSessionPointer(repoRoot string) (*SessionPointer, error) {
	dir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p SessionPointer
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func WriteSessionPointer(repoRoot string, p *SessionPointer) error {
	dir, err := ensureWeaveQueueDir(repoRoot)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "session.json")
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := weaveWriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
