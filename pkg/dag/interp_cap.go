// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// CapExecHandler returns an interp.ExecHandlers middleware that reports, for
// every command dispatched in a DAG target body, the effects the target's
// Effects: line did not declare — and then runs the command anyway.
//
// The cap is advisory. The check needs to know what a command does, and the
// only source for that is the atlas, a table bashy curates and can never
// complete: cc, ssh, outpost, a helper script, the shell re-entering itself
// have each broken a real target under a denying cap. Bashy ships the rod
// (the seam, the vocabulary, the report), not the fish (the law of what
// every tool in the world does) — so an undeclared or unclassified command
// is named ONCE per target on the body's stderr, and the body's exit status
// is the body's own:
//
//   - pure commands are never reported.
//   - commands whose atlas effects all fit within the cap pass silently.
//   - commands that exceed the cap are reported with the undeclared effects.
//   - commands absent from the atlas are reported as "unknown".
//
// No cap on the context (a target with no Effects) passes through silently.
//
// Ordering: interp.ExecHandlers chains middlewares first to last and the runner
// calls the FIRST, so this handler must be the first argument — outermost — in
// the chain. Placed before shell.Handler() it sees even the in-process
// coreutils tools that handler serves without calling next, and it applies
// identically to compound commands, pipelines, and subshells: every leaf
// command the shell dispatches through the ExecHandler seam is checked.
func CapExecHandler() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			tc, ok := ctx.Value(taskCapKey{}).(*taskCap)
			if !ok || len(args) == 0 {
				return next(ctx, args)
			}
			name, effects := classifyCommand(ctx, args)
			if undeclared := tc.cap.Exceeded(effects); len(undeclared) > 0 {
				key := name + "\x00" + strings.Join(undeclared, ",")
				if _, seen := tc.reported.LoadOrStore(key, struct{}{}); !seen {
					fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, capWarning(tc.target, name, undeclared, tc.cap))
				}
			}
			return next(ctx, args)
		}
	}
}

// taskCap is a target's parsed Effects: cap plus the once-per-target report
// ledger, carried under dag's own context key so the report is the ONLY
// consequence — no other handler (bashy's opt-in audit denies on the
// advice cap a @guard sets) can turn it into a denial.
type taskCap struct {
	target   string
	cap      advice.Cap
	reported sync.Map
}

type taskCapKey struct{}

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
// is <t>'s own declared Effects (none declared → unknown). A
// recursive call with no verb, a flag (`bashy -c …`) or a script path stays
// unknown: nothing declares what it does. The report names the verb: `"go"
// needs …`, not `"bashy"`.
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
// treats absence of a cap as nothing to report.
//
// A declaration that does not parse is the one hard failure left: the
// vocabulary is the rod's own grammar (BuildGraph rejects it at graph
// construction, so it means a Task assembled by hand). The error is returned
// so the caller can refuse to run the body; the returned context carries no
// cap.
func WithTaskCap(ctx context.Context, target string, effects []string) (context.Context, error) {
	if len(effects) == 0 {
		return ctx, nil
	}
	cap, err := parseEffects(effects)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, taskCapKey{}, &taskCap{target: target, cap: cap}), nil
}

// parseEffects is the ONE place a target's Effects: declaration is turned into
// a cap: BuildGraph validates through it and WithTaskCap enforces through it, so
// the accepted vocabulary is policy/advice's (the atlas effect atoms) by
// construction — dag keeps no vocabulary of its own.
func parseEffects(effects []string) (advice.Cap, error) {
	return advice.ParseCap(strings.Join(effects, ","))
}

// capWarning is the report for an undeclared effect: the target, the command,
// the effects it needs that the target did not declare, and the cap in force.
func capWarning(target, name string, undeclared []string, cap advice.Cap) string {
	return fmt.Sprintf("dag: effect cap: target %q: %q needs %s not in Effects: %s",
		target, name, strings.Join(undeclared, ","), cap)
}
