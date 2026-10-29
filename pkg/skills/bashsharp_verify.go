package skills

import (
	"context"
	"fmt"
	"io"

	"github.com/qiangli/yoke/pkg/shape/verify/repair"
)

// Learned-overlay verification. A repaired Bash# body reaches the
// host-local overlay only after the verifier's repair arm passes it
// against the contract frozen from the ORIGINAL unit body, and the pass is
// sealed in an evidence record bound to the overlay body's identity. The
// load path recomputes that binding independently: a stale, missing or
// failed record runs the authored body, never the overlay.

// overlayVerified reports whether a previously learned overlay may run: its
// decorators must still match the authored unit's, and a passing evidence
// record for exactly this overlay identity, base, contract, engine and
// coordinate must exist. Anything else runs the authored body.
func overlayVerified(cfg *config, name string, p runPrep, learned string, log io.Writer) bool {
	if bashSharpDecorators(learned) != bashSharpDecorators(p.body) {
		return false
	}
	if err := repair.RequireOverlay(cfg.cfgDir, p.body, learned, p.ctxKey); err != nil {
		fmt.Fprintf(log, "skills: %s: ignoring host overlay without passing evidence (%v) — running the authored unit\n", name, err)
		return false
	}
	return true
}

// sealBashSharpOverlay records the verified pass for candidate, bound to
// the overlay body's identity. Only a candidate VerifyRepair passed leads
// here; the seal makes that pass durable across runs.
func sealBashSharpOverlay(cfg *config, p runPrep, candidate string) error {
	return repair.SealOverlay(cfg.cfgDir, p.body, candidate, p.ctxKey)
}

// evalContractClause evaluates one frozen contract clause through the
// in-process userland: a nil error means the clause holds in dir. It is the
// candidate-independent evaluator VerifyRepair checks the frozen
// require/ensure clauses with.
func evalContractClause(ctx context.Context, dir, expr string) error {
	code, err := runShellCommand(ctx, dir, expr, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d", code)
	}
	return nil
}
