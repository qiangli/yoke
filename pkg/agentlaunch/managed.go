package agentlaunch

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
)

// resolveManaged points a managed tool's launch at its pinned cache path.
//
// The path is computed, not downloaded: Resolve is called from listings and
// dry runs that must not touch the network, and the consumers that actually
// spawn call EnsureManaged first. An operator override — the per-tool
// BASHY_TOOL_BINARY_<NAME> variable, or a `cli.binary:` path — wins, out loud:
// the launch then carries no ManagedTool and the override binary runs as
// given. There is no third case: a managed recipe never resolves to a bare
// PATH name.
func resolveManaged(l *Launch, tool fleet.Tool) error {
	if !tool.IsManaged() {
		return nil
	}
	if p, ok := tool.ManagedOverride(os.Getenv); ok {
		l.Tool, l.overridden = p, true
		return nil
	}
	bt, err := tool.BinmgrTool()
	if err != nil {
		return fmt.Errorf("agent launch: %w", err)
	}
	path, err := binmgr.InstallPath(bt)
	if err != nil {
		return fmt.Errorf("agent launch: tool %q managed install %s: %w (set %s to launch another binary explicitly)",
			tool.Name, bt.Version, err, fleet.ToolBinaryOverrideEnv(tool.Name))
	}
	l.Tool = path
	l.ManagedTool = &bt
	l.Env = append([]string(nil), tool.CLI.Managed.Env...)
	return nil
}

// EnsureManaged installs the launch's pinned tool on first use — download,
// digest-verify, cache — and returns the executable path. A cache hit costs
// no network. A launch without a managed install returns Tool unchanged.
//
// Failure is the launch's failure: there is deliberately no fallback to a
// PATH or Homebrew copy of the tool, because an unpinned binary is exactly
// what the pin exists to keep out of a measured run. The error names the
// override that lets an operator choose one explicitly.
func EnsureManaged(ctx context.Context, l Launch) (string, error) {
	if l.ManagedTool == nil {
		return l.Tool, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := binmgr.Ensure(ctx, *l.ManagedTool)
	if err != nil {
		return "", fmt.Errorf("agent launch: tool %q managed install %s: %w (no PATH fallback; set %s to launch another binary explicitly)",
			l.ToolName, l.ManagedTool.Version, err, fleet.ToolBinaryOverrideEnv(l.ToolName))
	}
	return path, nil
}

// ApplyLaunchEnv sets the launch's registry-declared KEY=VALUE pairs on a
// child environment, replacing any inherited value of the same name: the
// recipe's self-update switch must hold even when the operator's shell says
// otherwise, or the pin is not a pin.
func ApplyLaunchEnv(env []string, l Launch) []string {
	if len(l.Env) == 0 {
		return env
	}
	out := append([]string(nil), env...)
	for _, kv := range l.Env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		kept := out[:0]
		for _, e := range out {
			if !strings.HasPrefix(e, key+"=") {
				kept = append(kept, e)
			}
		}
		out = append(kept, kv)
	}
	return out
}

// ToolFailure reports the first stdout line the launch's recipe declares as a
// failure event, for a consumer that captured a clean exit and must decide
// whether to believe it.
func ToolFailure(l Launch, stdout []byte) (string, bool) {
	return l.FailEvents.FirstMatch(stdout)
}
