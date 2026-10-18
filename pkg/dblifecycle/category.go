package dblifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// Access is how a category operation opens its target. It is declared by the
// operation from its typed input, never inferred from query text.
type Access int

const (
	// AccessRead opens a read-only handle; the operation must also refuse
	// mutation on a held (read-write) handle.
	AccessRead Access = iota
	// AccessWrite is a serialized writer; readers continue.
	AccessWrite
	// AccessExclusive needs confirmation, the whole-database claim and the
	// backend-native exclusive lock (or the caller's current owner hold).
	AccessExclusive
)

// Outcome is a category operation's result body plus its bounded page.
type Outcome struct {
	Result   any      `json:"result,omitempty"`
	Page     any      `json:"page,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// CategoryOp is a category data operation (for example sql.query). It runs
// through the same claim guard, owner admission, request-id replay and hold
// epoch checks as the lifecycle verbs; a category package registers it at
// init, so this package never imports a category.
type CategoryOp struct {
	Access func(input json.RawMessage) (Access, error)
	Spec   func(mode dbstore.Mode, a Access) Operation
	Run    func(ctx context.Context, h dbstore.Handle, input json.RawMessage, a Access) (Outcome, error)
}

var categoryOps sync.Map // op name -> CategoryOp

// RegisterCategoryOp installs a category operation under its op name.
func RegisterCategoryOp(name string, op CategoryOp) { categoryOps.Store(name, op) }

// LookupCategoryOp resolves a registered category operation and its access
// for this input.
func LookupCategoryOp(name string, input json.RawMessage) (CategoryOp, Access, bool, error) {
	v, ok := categoryOps.Load(name)
	if !ok {
		return CategoryOp{}, 0, false, nil
	}
	op := v.(CategoryOp)
	a, err := op.Access(input)
	return op, a, true, err
}

// RunCategory runs a category operation on a direct (non-owner) path. Read
// and write operations open through Open's claim guard; an exclusive one
// takes the whole-database claim and the native exclusive lock first, like
// drop, and releases only what it newly acquired.
func (s Service) RunCategory(ctx context.Context, t dbstore.Target, name string, input json.RawMessage, yes bool, wait time.Duration) (dbstore.Identity, Outcome, error) {
	id, a, err := resolve(t)
	if err != nil {
		return id, Outcome{}, err
	}
	op, access, ok, err := LookupCategoryOp(name, input)
	if !ok {
		return id, Outcome{}, fmt.Errorf("%w: db operation %s", dbstore.ErrUnsupported, name)
	}
	if err != nil {
		return id, Outcome{}, err
	}
	if access != AccessExclusive {
		h, err := s.Open(ctx, t, access == AccessRead, wait)
		if err != nil {
			return id, Outcome{}, err
		}
		defer h.Close()
		out, err := op.Run(ctx, h, input, access)
		return id, out, err
	}
	if !yes && name != "fts.export" {
		return id, Outcome{}, fmt.Errorf("%w: %s is destructive; pass --yes", dbstore.ErrConfirmationRequired, name)
	}
	probe := s.options(id, wait, false)
	probe.ReadOnly = true
	h, err := a.Open(ctx, id.Target, probe)
	if err != nil {
		return id, Outcome{}, err
	}
	if err := h.Close(); err != nil {
		return id, Outcome{}, err
	}
	hold, err := acquireClaim(ctx, id, s.holder(), wait)
	if err != nil {
		return id, Outcome{}, err
	}
	defer hold.release()
	if err := s.guard(ctx, id); err != nil {
		return id, Outcome{}, err
	}
	h, err = a.Open(ctx, id.Target, s.options(id, wait, true))
	if err != nil {
		return id, Outcome{}, err
	}
	defer h.Close()
	out, err := op.Run(ctx, h, input, access)
	return id, out, err
}
