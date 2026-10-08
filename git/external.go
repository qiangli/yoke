package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// This file is the "one door" for host git (sprint 252, S252.6): every
// production call site that shells out to a system git binary comes
// through here, never a raw exec.Command, so the set of external-git
// users is enumerable with one grep.
//
// Two entries, one contract each:
//
//   - Exec is native-only: anything unrouted or unflagged returns
//     ErrUnsupported, never touching the host.
//   - RunExternal is external-capable: it runs the native tier first
//     and replays the verbatim argv against the host git binary ONLY on
//     ErrUnsupported. Callers that know they need the host (ls-remote,
//     auth-URL pushes) still come here — they get native automatically
//     the day those verbs route, with no call-site change.
//
// Both preserve exit codes in ExecResult. A missing host binary with an
// unrouted verb fails loud (128), never silently. Stdin is left alone
// (nil): interactive host-git prompts fail rather than hang the caller.

// RunExternal runs a git argv through the native tier, falling back to
// the host git binary with the verbatim argv on ErrUnsupported.
// Environment passes through (GIT_*, SSH, credential helpers all work).
func RunExternal(ctx context.Context, dir string, args []string) (*ExecResult, error) {
	res, err := Exec(ctx, dir, args)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, ErrUnsupported) {
		return nil, err
	}
	path, lerr := exec.LookPath("git")
	if lerr != nil {
		return &ExecResult{
			Stderr:   fmt.Sprintf("git: external execution requested for %q but no host git binary is on PATH\n", firstArg(args)),
			ExitCode: 128,
		}, nil
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		var exitErr *exec.ExitError
		if errors.As(rerr, &exitErr) {
			return &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitErr.ExitCode()}, nil
		}
		return nil, rerr
	}
	return &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "(no subcommand)"
}

// ForwardResult replays a captured result onto streams and maps failure
// to a Go error, so call sites that used to wire a subprocess's Stdout/
// Stderr live keep their output shape across the migration.
func ForwardResult(stdout, stderr io.Writer, args []string, res *ExecResult, runErr error) error {
	if res != nil {
		if res.Stdout != "" {
			fmt.Fprint(stdout, res.Stdout)
		}
		if res.Stderr != "" {
			fmt.Fprint(stderr, res.Stderr)
		}
	}
	if runErr != nil {
		return runErr
	}
	if res != nil && res.ExitCode != 0 {
		return fmt.Errorf("git %s: exit status %d", strings.Join(args, " "), res.ExitCode)
	}
	return nil
}

// RunChecked runs argv through RunExternal and maps a non-zero exit to
// a Go error carrying the verb, code and stderr tail, so migrated call
// sites keep their `if err != nil` shapes. Stdout comes back on success.
func RunChecked(ctx context.Context, dir string, args []string) (string, error) {
	res, err := RunExternal(ctx, dir, args)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("git %s: exit status %d: %s", strings.Join(args, " "), res.ExitCode, msg)
	}
	return res.Stdout, nil
}
