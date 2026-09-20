// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/coreutils/shell"
)

// bashppInterp runs a target body tagged ```bashpp (or ```bash++) as Bash++:
// the same in-process fork and coreutils userland as bashInterp, but parsed
// and executed under syntax.LangBashPP, so a body may use Go-shaped Bash++
// constructs and declare foreign source fences — `~~~py as py … ~~~` followed
// by `py.main()` — which the runner prepares through its polyglot seam
// (environment discovery starts from TaskIO.Dir, so the nearest project venv
// is selected). Untagged and ```bash bodies stay Classic on purpose: existing
// task files are never reinterpreted, and Bash++ is opted into per target.
//
// Authoring note: the dag parser closes a body only on a line equal to the
// OPENING fence marker, so a `~~~py … ~~~` block nests inside a ```bashpp
// recipe; a recipe that itself opens with `~~~` cannot contain one.
type bashppInterp struct{}

func (bashppInterp) Run(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	start := time.Now()
	res := TaskResult{Name: t.Name}

	prog, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(t.Body), t.Name)
	if err != nil {
		res.Status, res.ExitCode, res.Err = StatusFailed, 2, err
		res.Duration = time.Since(start)
		return res
	}

	runner, err := interp.New(
		interp.Lang(syntax.LangBashPP),
		interp.Dir(tio.Dir),
		interp.Env(expand.ListEnviron(tio.Env...)),
		interp.StdIO(nil, tio.Stdout, tio.Stderr),
		// CapExecHandler checks every dispatched command against the task's
		// declared-effects cap (set on ctx by WithTaskCap before Run).
		// It runs BEFORE shell.Handler so in-process coreutils commands are
		// covered the same as real binaries.  No cap on ctx → pass-through.
		interp.ExecHandlers(CapExecHandler(), shell.Handler()),
	)
	if err != nil {
		res.Status, res.ExitCode, res.Err = StatusFailed, 1, err
		res.Duration = time.Since(start)
		return res
	}

	runErr := runner.Run(ctx, prog)
	res.Duration = time.Since(start)
	res.ExitCode, res.Err = exitCodeFromErr(runErr)
	if res.ExitCode == 0 {
		res.Status = StatusDone
	} else {
		res.Status = StatusFailed
	}
	return res
}

func init() {
	bi := bashppInterp{}
	RegisterInterpreter("bashpp", bi)
	RegisterInterpreter("bash++", bi)
}
