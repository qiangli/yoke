// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/yoke/pkg/shape/selfbin"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// BashyDelegate is the production shaping agent: headless `bashy delegate`
// in genie mode, restricted to one allowed agent, in the candidate clone.
type BashyDelegate struct {
	// Bashy is the binary. Empty = "bashy" on PATH.
	Bashy string
	// Agent is the tool:model the delegation is pinned to, e.g. codex:<model>.
	Agent string
	// Timeout and IdleTimeout default to 20m and 5m.
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

// Delegate runs the agent and parses the bashy-chat-v1 envelope.
func (d BashyDelegate) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	if strings.TrimSpace(d.Agent) == "" {
		return DelegateResult{}, errors.New("no shaping agent configured (tool:model)")
	}
	bin := selfbin.Resolve(d.Bashy)
	timeout, idle := d.Timeout, d.IdleTimeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	if idle == 0 {
		idle = 5 * time.Minute
	}
	cmd := exec.CommandContext(ctx, bin, "delegate", "--json", "--agent", d.Agent,
		"--timeout", timeout.String(), "--idle-timeout", idle.String(), "--cwd", req.Dir, req.Task)
	cmd.Dir = req.Dir
	cmd.Env = delegateEnv(d.Agent)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	runErr := cmd.Run()
	var env struct {
		SchemaVersion string `json:"schema_version"`
		Agent         string `json:"agent"`
		Identity      string `json:"identity"`
		Receipt       string `json:"receipt"`
		ExitCode      int    `json:"exit_code"`
		Output        string `json:"output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil {
		if runErr != nil {
			return DelegateResult{}, fmt.Errorf("delegate: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return DelegateResult{}, fmt.Errorf("delegate: unreadable result envelope: %w", err)
	}
	res := DelegateResult{Agent: env.Agent, Identity: env.Identity, Receipt: env.Receipt, Output: env.Output}
	if res.Agent == "" {
		res.Agent = d.Agent
	}
	if runErr != nil || env.ExitCode != 0 {
		return res, fmt.Errorf("delegate: agent %s exited %d", res.Agent, env.ExitCode)
	}
	return res, nil
}
