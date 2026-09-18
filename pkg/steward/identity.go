// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"fmt"
	"os"
	"strconv"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// EpochEnv carries the holder's fencing token to its own child processes.
//
// A steward is not one process. It is a shell, its subagents, its hooks, and the
// commands it runs — and every one of them that writes to the journal must present
// the epoch of the tenure it belongs to. Exporting it at claim time is what makes
// that possible without threading a flag through everything.
//
// It is a TOKEN, and the value of a token is that it goes STALE. A zombie shell that
// was claimed under epoch 4, lapsed, and comes back to a host now at epoch 5 still
// has BASHY_STEWARD_EPOCH=4 in its environment — so its next write is fenced, loudly,
// instead of interleaving with its successor's. Re-deriving "whatever epoch is
// current" from the journal at write time would defeat exactly this, which is why
// there is no such shortcut.
const EpochEnv = "BASHY_STEWARD_EPOCH"

// selfRef resolves who this process is, DELEGATING to the coord identity rule rather
// than inventing a second one.
//
// This matters more than it looks. coord.Self mints and exports BASHY_EPISODE when
// the ambient environment has none — the gap that made two human-launched agent
// sessions mutually invisible. If steward resolved identity its own way, the same
// agent could be "session-a1b2c3" to the claim registry and something else to the
// seat, and the singleton would be enforced against a name that nothing else uses.
// One host, one steward, one way of saying who you are.
func selfRef() principal.Ref { return coord.Self() }

// ExportEpoch publishes the fencing token to this process and its children, so a
// steward's subagents and hooks inherit the tenure they are writing under.
func ExportEpoch(epoch uint64) error {
	return os.Setenv(EpochEnv, strconv.FormatUint(epoch, 10))
}

// EpochFromEnv reads the ambient fencing token. Zero means none is set — which is an
// absence, never a wildcard.
func EpochFromEnv() uint64 {
	v, err := strconv.ParseUint(os.Getenv(EpochEnv), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ResolveEpoch picks the fencing token a mutation will present: the explicit flag
// first, then the ambient one exported at claim time.
//
// It REFUSES to invent one. If the caller has no token, the honest answer is that
// they are not holding a tenure they can prove, and the fix is to claim the seat (or
// pass --epoch), not to let the write through on the assumption that whatever the
// journal currently says must be them.
func ResolveEpoch(flag uint64) (uint64, error) {
	if flag != 0 {
		return flag, nil
	}
	if e := EpochFromEnv(); e != 0 {
		return e, nil
	}
	return 0, fmt.Errorf("steward: no fencing epoch to present. Every authoritative write presents the epoch it "+
		"believes it holds — pass --epoch, or export $%s (`steward claim` prints the line to eval). "+
		"There is no 'whatever is current' shortcut: an agent that does not know its tenure ended is exactly the "+
		"agent that would write over its successor", EpochEnv)
}
