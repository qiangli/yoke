package dblifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func init() {
	coord.RegisterKind(coord.Kind{Name: "db", Match: coord.MatchName, Modes: []string{coord.ModeLease}})
}

type claimHold struct {
	ref    coord.Ref
	epoch  uint64
	new    bool
	holder principal.Ref
	claim  *coord.Claim
}

func acquireClaim(ctx context.Context, id dbstore.Identity, holder principal.Ref, wait time.Duration) (claimHold, error) {
	kind, name := id.ClaimUse()
	ref := coord.Ref{Kind: kind, Name: name}
	self := holder
	// A valid nested caller claim is heartbeated and left in place. An epoch
	// mismatch is never treated as an absent claim.
	if g, err := coord.Refresh(ctx, ref, self, 0); err == nil {
		return claimHold{ref: ref, epoch: g.Epoch, holder: self, claim: g.Claim}, nil
	} else if !errors.Is(err, coord.ErrNotHeld) {
		return claimHold{}, err
	}
	g, err := coord.AcquireRef(ctx, coord.Request{
		Ref: ref, Holder: self, Intent: "database whole-target exclusive use",
		Mode: coord.ModeLease, Wait: wait,
	})
	if err != nil {
		return claimHold{}, err
	}
	return claimHold{ref: ref, epoch: g.Epoch, new: true, holder: self, claim: g.Claim}, nil
}

func (h claimHold) release() error {
	if !h.new {
		return nil
	}
	return coord.ReleaseRef(context.Background(), h.ref, h.holder, h.epoch)
}

// Hold owns the backend lock and a current claim epoch. The caller must keep
// it alive and Release it; the epoch is checked at the enforcement point for
// requests that pass through this process. It does not claim distributed
// exclusion.
type Hold struct {
	Identity dbstore.Identity
	Epoch    uint64
	claim    claimHold
	handle   dbstore.Handle
}

func (s Service) AcquireHold(ctx context.Context, t dbstore.Target, wait time.Duration) (*Hold, error) {
	id, a, err := resolve(t)
	if err != nil {
		return nil, err
	}
	c, err := acquireClaim(ctx, id, s.holder(), wait)
	if err != nil {
		return nil, err
	}
	h, err := a.Open(ctx, id.Target, s.options(id, wait, true))
	if err != nil {
		_ = c.release() // backend lock failed: release only our new claim
		return nil, err
	}
	return &Hold{Identity: id, Epoch: c.epoch, claim: c, handle: h}, nil
}

// Check rejects a stale holder even when it still knows the owner endpoint.
func (h *Hold) Check(ctx context.Context, epoch uint64) error {
	if h == nil || epoch == 0 || epoch != h.Epoch {
		return coord.ErrFenced
	}
	_, err := coord.Refresh(ctx, h.claim.ref, h.claim.holder, epoch)
	return err
}

func (h *Hold) Release(ctx context.Context, epoch uint64) error {
	if h == nil || epoch == 0 || epoch != h.Epoch {
		return coord.ErrFenced
	}
	checkErr := h.Check(ctx, epoch)
	if checkErr != nil {
		// A stale holder must free its native lock even though it may not
		// release the now-current claim. ReleaseRef fences on the old epoch.
		closeErr := h.CloseBackend()
		releaseErr := h.claim.release()
		return errors.Join(checkErr, closeErr, releaseErr)
	}
	var closeErr error
	if h.handle != nil {
		closeErr = h.handle.Close()
	}
	releaseErr := h.claim.release()
	h.handle = nil
	if closeErr != nil || releaseErr != nil {
		return errors.Join(closeErr, releaseErr)
	}
	return nil
}

// CloseBackend frees the native lock while preserving the existing claim. It
// is used only inside an owner-serialized destructive operation; another
// Bashy client still sees the claim and cannot enter the removal window.
func (h *Hold) CloseBackend() error {
	if h.handle == nil {
		return nil
	}
	err := h.handle.Close()
	h.handle = nil
	return err
}

func (h *Hold) String() string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("%s@%d", h.Identity.ID, h.Epoch)
}
