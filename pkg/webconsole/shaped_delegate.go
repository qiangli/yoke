// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	shapecore "github.com/qiangli/yoke/pkg/shape"
	"os"
	"runtime"
	"time"
)

// BashyDelegate is the production shaping agent: headless `bashy delegate`
// in genie mode, restricted to one allowed agent, in the candidate clone.
type BashyDelegate struct {
	// Bashy is the binary. Empty = "bashy" on PATH.
	Bashy string
	// Agent is the tool:model the delegation is pinned to, e.g. codex:<model>.
	Agent string
	// Timeout and IdleTimeout default to 20m and 15m (delegateTimeouts).
	Timeout     time.Duration
	IdleTimeout time.Duration
}

// delegateEnvAllow is everything the agent process inherits. The agent needs
// to find its own tooling and its own provider login; it never receives the
// console's environment wholesale, so no principal secret, session key or
// app data location can ride along.
var delegateEnvAllow = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TERM", "BASHY_HOME"}

// delegateEnvAllowWindows is what a Windows process needs on top of that to
// locate its profile, state and temp directories and to start at all — the
// Windows spelling of HOME/TMPDIR, plus the system root and executable
// lookup. None of these carries a secret.
var delegateEnvAllowWindows = []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "SystemRoot", "PATHEXT", "ComSpec"}

// delegateEnvNames is the allow-list for goos.
func delegateEnvNames(goos string) []string {
	if goos == "windows" {
		return append(append([]string(nil), delegateEnvAllow...), delegateEnvAllowWindows...)
	}
	return delegateEnvAllow
}

func delegateEnv(agent string) []string {
	var env []string
	for _, k := range delegateEnvNames(runtime.GOOS) {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "BASHY_DELEGATE_CALLER=genie", "BASHY_DELEGATE_ALLOW="+agent)
}

// delegateTimeouts fills the defaults. The idle watchdog counts agent output,
// and one model turn over a real app's tree (the ~340 KB launcher) can be
// silent for several minutes, so idle must outlast a single turn.
func (d BashyDelegate) delegateTimeouts() (timeout, idle time.Duration) {
	timeout, idle = d.Timeout, d.IdleTimeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	if idle == 0 {
		idle = 15 * time.Minute
	}
	return timeout, idle
}

// Delegate runs the agent and parses the bashy-chat-v1 envelope.
func (d BashyDelegate) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	return (shapecore.BashyDelegate{Bashy: d.Bashy, Agent: d.Agent, Timeout: d.Timeout, IdleTimeout: d.IdleTimeout}).Delegate(ctx, req)
}
