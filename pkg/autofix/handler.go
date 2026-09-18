package autofix

import (
	"context"
	"encoding/json"
	"fmt"

	"mvdan.cc/sh/v3/interp"

	"github.com/qiangli/coreutils/pkg/nudge"
	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Handler is an interp.ExecHandler middleware that adapts a read-only command
// with a wrong-dialect/platform flag into the local equivalent BEFORE it runs,
// emitting a note to the command's own stderr so the agent sees — in the SAME
// tool result — that its command was adapted and why. That is the round-trip it
// saves: a result with a note instead of an error to diagnose and retry.
//
// Wire it AFTER permission validation and BEFORE the coreutils/fork handlers, so
// the adapted argv is what actually executes. Rewriting requires explicit
// agentic mode and an enabled disclosure channel; ambient agent detection alone
// never changes argv.
func Handler() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if !weavecli.IsAgent() || !nudge.Enabled() {
				return next(ctx, args)
			}
			fixed, note, ok := Adapt(args)
			if !ok {
				return next(ctx, args)
			}
			emit(ctx, note, fixed)
			return next(ctx, fixed)
		}
	}
}

type line struct {
	Schema string   `json:"schema_version"`
	Kind   string   `json:"kind"` // "autofix"
	Note   string   `json:"note"`
	Ran    []string `json:"ran"`
	Off    string   `json:"off"`
}

// emit writes the note to the command's captured stderr (interp.HandlerCtx), so
// it rides back to the agent in the tool result rather than the host terminal.
func emit(ctx context.Context, note string, ran []string) {
	hc := interp.HandlerCtx(ctx)
	if hc.Stderr == nil {
		return
	}
	if weavecli.IsAgentDriven() {
		b, _ := json.Marshal(line{
			Schema: nudge.SchemaVersion,
			Kind:   "autofix",
			Note:   note,
			Ran:    ran,
			Off:    "BASHY_HINTS=off",
		})
		fmt.Fprintf(hc.Stderr, "%s\n", b)
		return
	}
	fmt.Fprintf(hc.Stderr, "─── bashy autofix ─── %s (ran: %v) (silence: BASHY_HINTS=off)\n", note, ran)
}
