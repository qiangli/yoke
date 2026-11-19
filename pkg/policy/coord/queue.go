package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Waiter is a bounded request in the host-local claim ledger. Order in the
// ledger is FIFO, including overlapping path/member sets. It grants no authority.
type Waiter struct {
	ID         string        `json:"id"`
	Ref        Ref           `json:"ref"`
	Kind       Kind          `json:"kind"`
	Members    []string      `json:"members,omitempty"`
	Holder     principal.Ref `json:"holder"`
	Intent     string        `json:"intent,omitempty"`
	EnqueuedAt time.Time     `json:"enqueued_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
}

type QueueRow struct {
	Waiter
	Position int    `json:"position"`
	Reason   string `json:"reason"`
	Blocker  *Claim `json:"blocker,omitempty"`
	Ahead    string `json:"ahead,omitempty"`
}

var ErrQueueCanceled = errors.New("claim: wait ticket canceled or expired")

type QueueBlocked struct {
	ID     string
	Waiter Waiter
}

func (e *QueueBlocked) Error() string { return "claim: waiting behind ticket " + e.ID }

// Existing auto-acquisition guards distinguish Conflict from an unavailable
// ledger (which they intentionally fail open). A queued refusal is a real
// conflict, not a storage outage; keep that contract without inventing a hold.
func (e *QueueBlocked) Unwrap() error {
	w := e.Waiter
	return &Conflict{Queue: &w, Claim: &Claim{Kind: w.Kind.Name, Resource: w.Ref.Name, Members: w.Members, Holder: w.Holder, Intent: w.Intent, AcquiredAt: w.EnqueuedAt}}
}

// queue files share claims.lock; the non-json suffix prevents legacy claim
// readers from mistaking tickets for grants. Malformed queues fail closed.
func readQueue(dir string) ([]Waiter, error) {
	b, err := os.ReadFile(filepath.Join(dir, "waiters.queue"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Waiter
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("claim queue: %w", err)
	}
	return rows, nil
}
func writeQueue(dir string, rows []Waiter) error {
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "waiters.queue")
	if err := os.WriteFile(p+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}
func (w Waiter) party() party {
	return party{kind: w.Kind, name: w.Ref.Name, members: w.Members, set: w.Ref.Kind != w.Kind.Name}
}
func (s store) enqueue(sp acquireSpec, deadline time.Time) (string, error) {
	ref := mapBare(normRef(sp.req.Ref))
	k := effectiveKind(ref)
	members, err := resolveMembers(k, ref, sp.req.Members)
	if sp.legacy {
		ref = Ref{Kind: kindRepo}
		k = effectiveKind(ref)
		members = sp.roots
		err = nil
	}
	if err != nil {
		return "", err
	}
	if !sp.legacy && ref.Name == "" {
		return "", fmt.Errorf("claim: resource name is required")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	err = withDirLock(s.dir, func() error {
		rows, err := readQueue(s.dir)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		live := rows[:0]
		for _, w := range rows {
			if now.Before(w.ExpiresAt) {
				live = append(live, w)
			}
		}
		live = append(live, Waiter{ID: id, Ref: ref, Kind: k, Members: members, Holder: sp.req.Holder, Intent: sp.req.Intent, EnqueuedAt: now, ExpiresAt: deadline})
		return writeQueue(s.dir, live)
	})
	return id, err
}

// CancelWait removes only the caller's ticket. It never releases a grant.
func CancelWait(dir, id string, holder principal.Ref) error {
	return withDirLock(dir, func() error {
		rows, err := readQueue(dir)
		if err != nil {
			return err
		}
		for i, w := range rows {
			if w.ID == id {
				if !sameHolder(w.Holder, holder) {
					return fmt.Errorf("claim: wait ticket belongs to another owner")
				}
				return writeQueue(dir, append(rows[:i], rows[i+1:]...))
			}
		}
		return nil
	})
}

// queueTurn runs INSIDE the file backend acquisition transaction. A non-waiting
// newcomer cannot jump the queue. Existing holders may still refresh/release.
func (s store) queueTurn(id string, want party, now time.Time) error {
	rows, err := readQueue(s.dir)
	if err != nil {
		return err
	}
	if id != "" {
		found := false
		for _, w := range rows {
			if w.ID == id && now.Before(w.ExpiresAt) {
				found = true
			}
		}
		if !found {
			return ErrQueueCanceled
		}
	}
	for _, w := range rows {
		if !now.Before(w.ExpiresAt) {
			continue
		}
		if w.ID == id {
			return nil
		}
		if kindsConflict(want, w.party()) {
			return &QueueBlocked{ID: w.ID, Waiter: w}
		}
	}
	return nil
}

// Queue reports current holders and earlier conflicting tickets from one locked
// snapshot. Expired tickets are omitted; unknown holders remain blockers.
func Queue(dir string) ([]QueueRow, error) {
	out := []QueueRow{}
	err := withDirLock(dir, func() error {
		rows, err := readQueue(dir)
		if err != nil {
			return err
		}
		claims, err := listFiles(dir)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, w := range rows {
			if !now.Before(w.ExpiresAt) {
				continue
			}
			row := QueueRow{Waiter: w, Position: 1, Reason: "ready; awaiting acquisition"}
			for _, prior := range out {
				if kindsConflict(w.party(), prior.Waiter.party()) {
					row.Position++
					if row.Ahead == "" {
						row.Ahead = prior.ID
						row.Reason = "earlier conflicting ticket"
					}
				}
			}
			for _, c := range claims {
				if !c.Stale(now) && kindsConflict(w.party(), claimParty(c)) {
					row.Blocker = c
					row.Reason = "held by " + c.Holder.Name
					break
				}
			}
			out = append(out, row)
		}
		return nil
	})
	return out, err
}

func (s store) attachedWait(ctx context.Context, r Request) (Grant, *lockfile.Lock, error) {
	deadline := time.Now().Add(r.Wait)
	wait := r.Wait
	if wait > 0 && r.Force {
		return Grant{}, nil, fmt.Errorf("claim: --force cannot join a queue")
	}
	r.Wait = 0 // every kernel-lock attempt is nonblocking and context cancellable
	id := ""
	if wait > 0 {
		if customBackend(mapBare(normRef(r.Ref)).Kind) != nil {
			return Grant{}, nil, fmt.Errorf("claim: queued attached holds require the host-local backend")
		}
		var err error
		id, err = s.enqueue(acquireSpec{req: r}, deadline)
		if err != nil {
			return Grant{}, nil, err
		}
		defer CancelWait(s.dir, id, r.Holder)
	}
	for {
		if err := ctx.Err(); err != nil {
			return Grant{}, nil, err
		}
		if err := withDirLock(s.dir, func() error { return s.checkTicket(id) }); err != nil {
			return Grant{}, nil, err
		}
		g, l, err := s.acquireAttachedTicket(r, id)
		var conflict *Conflict
		var queued *QueueBlocked
		if err == nil || wait <= 0 || (!errors.As(err, &conflict) && !errors.As(err, &queued)) || !time.Now().Before(deadline) {
			return g, l, err
		}
		select {
		case <-ctx.Done():
			return Grant{}, nil, ctx.Err()
		case <-time.After(min(100*time.Millisecond, time.Until(deadline))):
		}
	}
}

// checkTicket is called under claims.lock, including the grant transaction.
func (s store) checkTicket(id string) error {
	if id == "" {
		return nil
	}
	rows, err := readQueue(s.dir)
	if err != nil {
		return err
	}
	for _, w := range rows {
		if w.ID == id && time.Now().Before(w.ExpiresAt) {
			return nil
		}
	}
	return ErrQueueCanceled
}
