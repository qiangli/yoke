// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"errors"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/coreutils/shell"
)

// bashInterp runs a target body through the in-process mvdan.cc/sh/v3 fork with
// the coreutils userland wired in (shell.Handler()), so cat/ls/grep/yc/… and
// any other registered tool resolve in-process with no subprocess. A fresh
// runner per task gives make-style per-recipe isolation (no shared shell state).
type bashInterp struct{}

func (bashInterp) Run(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	start := time.Now()
	res := TaskResult{Name: t.Name}

	prog, err := syntax.NewParser().Parse(strings.NewReader(t.Body), t.Name)
	if err != nil {
		res.Status, res.ExitCode, res.Err = StatusFailed, 2, err
		res.Duration = time.Since(start)
		return res
	}

	runner, err := interp.New(
		interp.Dir(tio.Dir),
		interp.Env(expand.ListEnviron(tio.Env...)),
		interp.StdIO(nil, tio.Stdout, tio.Stderr),
		// coreutils userland first; misses fall through to the default exec
		// handler (real binaries: go, docker, …), so build DAGs work too.
		interp.ExecHandlers(shell.Handler()),
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

// exitCodeFromErr maps a runner.Run error to a (code, err). A clean run is
// (0, nil); an interp.ExitStatus carries the shell exit code with no Go error;
// anything else is a fatal interpreter error reported as code 1.
func exitCodeFromErr(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var st interp.ExitStatus
	if errors.As(err, &st) {
		return int(st), nil
	}
	return 1, err
}

func init() {
	bi := bashInterp{}
	RegisterInterpreter("", bi)
	RegisterInterpreter("bash", bi)
	RegisterInterpreter("sh", bi)
	RegisterInterpreter("shell", bi)
}
