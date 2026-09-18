package chat

import (
	"context"
	"os"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
)

// ProbeAgent launches an agent on a trivial prompt and reports whether it can
// actually speak.
//
// It goes through Invoke — the SAME path a real turn takes: the same argv from
// the registry, the same launch guard, the same read-only stripping, the same
// child environment. A probe that launched differently from production could
// pass while production failed, which would be worse than no probe at all: it
// would make a dead binding look verified.
//
// ReadOnly because a probe asks one question — can this binding speak — and
// answering it needs no write authority. That also means the probe passes the
// launch guard by construction on an ordinary uncontained host, rather than
// demanding the operator weaken it just to find out whether their fleet works.
// (Meeting turns used to say the same thing and no longer do: a seat now acts.
// See pkg/meet's turnAuthority. A probe is not a seat.)
//
// It runs in a scratch directory. A probe must not be able to touch the caller's
// repo, and a first-run trust prompt fires per-directory — probing in a temp dir
// is closer to the worst case a real run will meet.
func ProbeAgent(ctx context.Context, agent string, timeout time.Duration) (agentctl.ProbeStatus, string) {
	if timeout <= 0 {
		// Generous on purpose. A probe that times out reports a WORKING agent as
		// broken, and a false alarm in a fleet health check is worse than a slow
		// one: it sends an operator to re-peg a model that was fine. aider through
		// a metered API routinely takes a minute on a one-word answer.
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := ""
	if d, err := os.MkdirTemp("", "bashy-probe-"); err == nil {
		cwd = d
		defer os.RemoveAll(d)
	}

	res, err := Invoke(ctx, Options{
		Agent:       agent,
		Instruction: agentctl.ProbePrompt,
		Cwd:         cwd,
		Timeout:     timeout,
		ReadOnly:    true,
	}, nil)

	// The output is what gets classified, not the error. A tool that rejects a
	// model exits non-zero AND says why; the exit code alone cannot tell that
	// apart from a network blip, and the difference is the whole point of the
	// probe.
	out := res.Output
	if err != nil && out == "" {
		out = err.Error()
	}
	return agentctl.Classify(out, ctx.Err() == context.DeadlineExceeded)
}
