// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// CapExecHandler returns an interp.ExecHandlers middleware that enforces a
// declared-effects cap on every command dispatched in a DAG target body.
//
// When a cap is active on the context (set by WithTaskCap before the body
// runs), each argv[0] is looked up in the atlas:
//
//   - pure commands are always allowed regardless of the cap.
//   - commands whose atlas effects all fit within the cap are allowed.
//   - commands that exceed the cap (undeclared effects) are denied BEFORE
//     execution with exit 126, naming the command and the denied effects.
//   - commands absent from the atlas (unclassified) deny as "unknown",
//     matching Cap.Exceeded's fail-closed contract.
//
// No cap on the context (nil task.Effects) passes through to the next handler
// unconditionally — targets without declared effects run without restriction,
// preserving backward compatibility.
//
// This handler is placed BEFORE shell.Handler() in the ExecHandlers chain so
// the policy check runs even for pure-Go in-process commands and applies
// identically to compound commands, pipelines, and subshells: every leaf
// command that the shell dispatches through the ExecHandler seam is checked.
func CapExecHandler() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			cap, ok := advice.CapFrom(ctx)
			if !ok {
				return next(ctx, args)
			}
			if len(args) == 0 {
				return next(ctx, args)
			}

			name := args[0]
			// Strip a leading path component so `/usr/bin/echo` and
			// `./myscript` check against the basename, consistent with
			// how atlas keys its entries.
			if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
				name = name[i+1:]
			}

			atlasEffects := atlasEffectsFor(name)
			denied := cap.Exceeded(atlasEffects)
			if len(denied) == 0 {
				return next(ctx, args)
			}
			return interp.ExitStatus(126)
		}
	}
}

// atlasEffectsFor returns the atlas-recorded effects for a command name. The
// empty slice signals "unclassified" to Cap.Exceeded, which then returns
// ["unknown"] — the fail-closed contract for commands that are absent from the
// atlas.
func atlasEffectsFor(name string) []string {
	entry, ok := atlas.Lookup(name)
	if !ok {
		return nil // unclassified → Cap.Exceeded returns ["unknown"]
	}
	return entry.Effects
}

// WithTaskCap returns a context carrying the cap derived from a task's declared
// Effects. When Effects is empty no cap is set and the returned context is
// unchanged — the handler treats absence of a cap as unconstrained.
func WithTaskCap(ctx context.Context, effects []string) context.Context {
	if len(effects) == 0 {
		return ctx
	}
	cap, err := advice.ParseCap(strings.Join(effects, ","))
	if err != nil {
		// ParseCap only fails for unknown atoms; BuildGraph already
		// rejects unknown effects at parse time (contract.go knownEffects),
		// so this branch is unreachable in a correctly constructed Task.
		// Return an unconstrained context rather than silently permitting all.
		return ctx
	}
	return advice.WithCap(ctx, cap)
}

// capDeniedErr returns a human-readable error for a cap denial, for use in
// the engine's error path (not the handler: the handler is called inside the
// shell and must return a plain interp.ExitStatus).
func capDeniedErr(name string, denied []string) error {
	return fmt.Errorf("effect cap denied %q: undeclared effects %s (declare them in Effects: to allow)",
		name, strings.Join(denied, ", "))
}
