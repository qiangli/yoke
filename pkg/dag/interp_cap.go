// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
			name, effects := classifyCommand(ctx, args)
			denied := cap.Exceeded(effects)
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

// classifyCommand names the command a cap decision is about and returns its
// effects. A plain command is its basename looked up in the atlas. The shell
// re-entering itself — a body's `"$BASHY_EXE" go build`, `bashy git …`,
// `bashy dag <t>` — is not an atlas tool, so it is classified by its VERB:
// `bashy go` is the atlas entry for go, `bashy git` for git, and `bashy dag <t>`
// is <t>'s own declared Effects (none declared → unknown, fail closed). A
// recursive call with no verb, a flag (`bashy -c …`) or a script path stays
// unknown: nothing declares what it does. The diagnostic names the verb, so a
// denial reads `denied "go"`, not `denied "bashy"`.
func classifyCommand(ctx context.Context, args []string) (string, []string) {
	name := commandName(args[0])
	if !isSelf(args[0], name) {
		return name, atlasEffectsFor(name)
	}
	if len(args) < 2 {
		return name, nil
	}
	verb := args[1]
	if strings.HasPrefix(verb, "-") || strings.ContainsAny(verb, `/\`) {
		return name, nil
	}
	if verb == "dag" {
		if len(args) < 3 {
			return verb, nil
		}
		if resolve, ok := ctx.Value(targetEffectsKey{}).(func(string) ([]string, bool)); ok {
			if effects, ok := resolve(args[2]); ok && len(effects) > 0 {
				return verb + " " + args[2], effects
			}
		}
		return verb + " " + args[2], nil
	}
	return verb, atlasEffectsFor(verb)
}

// isSelf reports whether argv[0] is this shell: the file the runner exports as
// BASHY_EXE (the resolved running binary — its basename may be anything on a
// host that renamed it), else a bare bashy/bash name, with or without the
// Windows or launcher suffix.
func isSelf(arg0, name string) bool {
	if exe := os.Getenv("BASHY_EXE"); exe != "" && strings.ContainsAny(arg0, `/\`) {
		if a, err := filepath.Abs(arg0); err == nil && sameFile(a, exe) {
			return true
		}
	}
	base := strings.ToLower(name)
	for _, suffix := range []string{".exe", ".real"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base == "bashy" || base == "bash"
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// targetEffectsKey carries the run's target → declared Effects resolver so a
// body's `bashy dag <t>` can be classified by <t>'s declaration.
type targetEffectsKey struct{}

// WithTargetEffects attaches the resolver for `bashy dag <t>` classification.
func WithTargetEffects(ctx context.Context, resolve func(string) ([]string, bool)) context.Context {
	return context.WithValue(ctx, targetEffectsKey{}, resolve)
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
