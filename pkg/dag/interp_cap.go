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

// CapDeniedStatus is the shell status a body sees when a dispatched command
// exceeds the target's declared Effects: 126 ("found but not executable"), the
// same status the shell yields for a command it refuses to run.
const CapDeniedStatus = 126

// CapExecHandler returns an interp.ExecHandlers middleware that enforces a
// declared-effects cap on every command dispatched in a DAG target body.
//
// When a cap is active on the context (set by WithTaskCap before the body
// runs), each argv[0] is looked up in the atlas:
//
//   - pure commands are always allowed regardless of the cap.
//   - commands whose atlas effects all fit within the cap are allowed.
//   - commands that exceed the cap (undeclared effects) are denied BEFORE
//     execution with CapDeniedStatus, naming the command and the denied effects
//     on the body's stderr.
//   - commands absent from the atlas (unclassified) deny as "unknown",
//     matching Cap.Exceeded's fail-closed contract.
//
// No cap on the context (a target with no Effects) passes through to the next
// handler unconditionally — targets without declared effects run without
// restriction, preserving backward compatibility.
//
// Ordering: interp.ExecHandlers chains middlewares first to last and the runner
// calls the FIRST, so this handler must be the first argument — outermost — in
// the chain. Placed before shell.Handler() it runs even for the in-process
// coreutils tools that handler serves without calling next, and it applies
// identically to compound commands, pipelines, and subshells: every leaf
// command the shell dispatches through the ExecHandler seam is checked.
func CapExecHandler() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			cap, ok := advice.CapFrom(ctx)
			if !ok || len(args) == 0 {
				return next(ctx, args)
			}
			name := commandName(args[0])
			denied := cap.Exceeded(atlasEffectsFor(name))
			if len(denied) == 0 {
				return next(ctx, args)
			}
			fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, capDeniedError(name, denied, cap))
			return interp.ExitStatus(CapDeniedStatus)
		}
	}
}

// commandName strips a leading path so `/usr/bin/echo` and `./myscript` check
// against the basename, consistent with how the atlas keys its entries.
func commandName(arg0 string) string {
	if i := strings.LastIndexAny(arg0, "/\\"); i >= 0 {
		return arg0[i+1:]
	}
	return arg0
}

// atlasEffectsFor returns the atlas-recorded effects for a command name. nil
// signals "unclassified" to Cap.Exceeded, which then returns ["unknown"] — the
// fail-closed contract for commands absent from the atlas.
func atlasEffectsFor(name string) []string {
	entry, ok := atlas.Lookup(name)
	if !ok {
		return nil
	}
	return entry.Effects
}

// WithTaskCap returns a context carrying the cap derived from a task's declared
// Effects. Empty Effects sets no cap and returns ctx unchanged — the handler
// treats absence of a cap as unconstrained.
//
// Fail closed: when Effects does not parse (BuildGraph rejects that at graph
// construction, so it means a Task assembled by hand), the returned context
// carries the zero Cap — which denies every non-pure command — and the error
// is returned so the caller can refuse to run the body at all. The original
// unconstrained ctx is never returned for a non-empty declaration.
func WithTaskCap(ctx context.Context, effects []string) (context.Context, error) {
	if len(effects) == 0 {
		return ctx, nil
	}
	cap, err := parseEffects(effects)
	if err != nil {
		return advice.WithCap(ctx, advice.Cap{}), err
	}
	return advice.WithCap(ctx, cap), nil
}

// parseEffects is the ONE place a target's Effects: declaration is turned into
// a cap: BuildGraph validates through it and WithTaskCap enforces through it, so
// the accepted vocabulary is policy/advice's (the atlas effect atoms) by
// construction — dag keeps no vocabulary of its own.
func parseEffects(effects []string) (advice.Cap, error) {
	return advice.ParseCap(strings.Join(effects, ","))
}

// capDeniedError is the diagnostic for a cap denial: the command, the effects
// it needs that the target did not declare, and the cap that was in force.
func capDeniedError(name string, denied []string, cap advice.Cap) error {
	return fmt.Errorf("dag: effect cap denied %q: undeclared effects %s (declared: %s; add them to Effects: to allow)",
		name, strings.Join(denied, ","), cap)
}
