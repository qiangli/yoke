// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleetkinds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/fleet"
)

// ProbeState is what a resourcekind probe hook reports about a name.
type ProbeState string

const (
	// ProbeFree: the probe exited 0 — the name exists and nothing holds it.
	ProbeFree ProbeState = "free"
	// ProbeBusy: the probe exited 1 — the name exists and something holds it.
	ProbeBusy ProbeState = "busy"
	// ProbeUnknown: the probe exited anything else — nothing can be said.
	ProbeUnknown ProbeState = "unknown"
)

// probeStateFromExit maps a probe's exit code to its contract: 0 free, 1
// busy, anything else unknown.
func probeStateFromExit(code int) ProbeState {
	switch code {
	case 0:
		return ProbeFree
	case 1:
		return ProbeBusy
	default:
		return ProbeUnknown
	}
}

// hookTimeout bounds one hook run. Hooks execute on the claim path, where an
// unbounded wait would wedge an acquisition behind a hung helper.
const hookTimeout = 30 * time.Second

// lookupHookCommand resolves a hook to the registered command it names. A
// hook that names nothing registered is refused here — hooks never fall back
// to PATH, so a typo fails closed instead of running whatever the shell
// would have found.
func lookupHookCommand(hook string) (fleet.Command, error) {
	rec, ok := fleet.New().Command(hook)
	if !ok {
		return fleet.Command{}, fmt.Errorf("fleetkinds: hook %q is not a registered command — add it with `bashy commands add` (resource hooks never run PATH programs)", hook)
	}
	return rec, nil
}

// runHookCommand runs a hook command with args and returns its stdout and
// exit code. Only exec records run here: a download would provision over the
// network on the claim path, and a script needs the embedding shell to
// re-enter — both are refused with a clear error instead of half-working.
func runHookCommand(ctx context.Context, rec fleet.Command, args ...string) ([]byte, int, error) {
	if rec.Mode() != atlas.RegisteredExec {
		return nil, 0, fmt.Errorf("fleetkinds: hook %q is a %s command; resource hooks run exec records only", rec.Name, rec.Mode())
	}
	argv := rec.Argv("", "", args)
	if len(argv) == 0 {
		return nil, 0, fmt.Errorf("fleetkinds: hook %q has no argv", rec.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), exitErr.ExitCode(), nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, 0, fmt.Errorf("fleetkinds: hook %q timed out after %s", rec.Name, hookTimeout)
		}
		return nil, 0, fmt.Errorf("fleetkinds: hook %q did not run: %w", rec.Name, err)
	}
	return stdout.Bytes(), 0, nil
}

// runResolve turns a name into members through a resolve hook: the registered
// command it names, run with the name as its argument, one member per
// whitespace-trimmed output line. Empty output claims just the name itself,
// the same fallback an absent hook gets.
func runResolve(hook, name string) ([]string, error) {
	rec, err := lookupHookCommand(hook)
	if err != nil {
		return nil, err
	}
	out, _, err := runHookCommand(context.Background(), rec, name)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			members = append(members, line)
		}
	}
	if len(members) == 0 {
		return []string{name}, nil
	}
	return members, nil
}

// runProbe tests a name through a probe hook: the registered command it
// names, run with the name as its argument. Exit 0 is free, 1 is busy,
// anything else is unknown.
func runProbe(hook, name string) (ProbeState, error) {
	rec, err := lookupHookCommand(hook)
	if err != nil {
		return ProbeUnknown, err
	}
	_, code, err := runHookCommand(context.Background(), rec, name)
	if err != nil {
		return ProbeUnknown, err
	}
	return probeStateFromExit(code), nil
}
