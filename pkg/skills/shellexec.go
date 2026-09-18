package skills

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	coreshell "github.com/qiangli/coreutils/shell"
)

// runShellCommand executes one bound command line through the in-process
// interpreter with the coreutils userland mounted (Tier-1: a registered
// tool is a Go function call, no fork). This is the deterministic L3
// leaf the skills mechanism binds dhnt predicates/steps to.
func runShellCommand(ctx context.Context, dir, src string, out, errW io.Writer) (int, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "skill-command")
	if err != nil {
		return -1, err
	}
	r, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(strings.NewReader(""), out, errW),
		interp.ExecHandlers(coreshell.Handler()),
	)
	if err != nil {
		return -1, err
	}
	if err := r.Run(ctx, file); err != nil {
		var status interp.ExitStatus
		if errors.As(err, &status) {
			return int(status), nil
		}
		return -1, err
	}
	return 0, nil
}

// commandTimeout bounds each bound command (the run itself has no global
// deadline in P2 — each leaf does).
const commandTimeout = 10 * time.Minute
