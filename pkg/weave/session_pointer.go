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
	// RepoRemote is the remote the key was read from: `upstream` in the fork
	// layout `gh repo fork --clone` leaves behind, else `origin`.
	RepoRemote string `json:"repo_remote,omitempty"`
	// SprintSeq is the manager's sprint number bound to this session, so a
	// commit on another host may carry `Sprint: #<seq>` for a sprint that
	// exists on the manager's board only.
	SprintSeq int64 `json:"sprint_seq,omitempty"`
	// Role is the seat cloudbox gave this account on the session at the
	// last join: owner · contributor · observer. An observer can read the
	// board and send/receive mail; it cannot steer or take the lease.
	Role string `json:"role,omitempty"`
	// SeatAsOf is when Role was last derived from GitHub through
	// join-by-repo (RFC 3339, UTC). Empty for owner/member seats and for
	// pointers written before the seat was resynced per verb.
	SeatAsOf string `json:"seat_as_of,omitempty"`
	// Seats are the participants (`<name>@<host>`) this checkout has joined
	// the session AS. The join used to happen once per checkout — for
	// whoever ran the first verb — so a second seat on the same host (a
	// live agent session beside the person) never reached the roster and
	// could not be addressed from another host (sprint 220).
	Seats []string `json:"seats,omitempty"`
}

// GitHubSeated reports whether the seat is GitHub's answer (contributor or
// observer on a repo-keyed session) and so must be re-derived per verb —
// as opposed to the owner or a hand-shared member, whose seat is cloudbox's own.
func (p *SessionPointer) GitHubSeated() bool {
	return p != nil && p.RepoKey != "" && (p.Role == "contributor" || p.Role == "observer")
}

// sessionPointerPath is where the pointer lives, for messages that tell the
// reader what to remove.
func sessionPointerPath(repoRoot string) string {
	dir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return "the session pointer"
	}
	return filepath.Join(dir, "session.json")
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
