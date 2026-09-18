// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// meshExecutor runs a `Host:`-tagged target's body on another machine over a
// configurable remote-exec command (default "ssh"), feeding the body to a remote
// `bash -s` on stdin. It is the control-plane half of dag mesh execution: it
// transports the COMMAND + placement only. The body is responsible for fetching
// its own code/data from the DATA plane (GitHub for repos, an object store for
// artifacts) — never through this channel (see docs/dag-mesh-data-plane.md).
// A target with no Host runs locally, so a single mesh run mixes local and
// remote targets freely.
//
// The remote transport is a command template, so this works against a reachable
// host today (ssh) and a future outpost-tunnel dispatcher can replace it without
// touching the engine — the Executor seam is the stable boundary.
type meshExecutor struct {
	Remote      string // e.g. "ssh" or "ssh -p 2222 -i key"; empty => "ssh"
	RemoteShell string // e.g. "bash -s"; "none" => feed body directly to remote command
}

func (x meshExecutor) Execute(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	if strings.TrimSpace(t.Host) == "" {
		return localExecutor{}.Execute(ctx, t, tio) // no placement -> run here
	}
	start := time.Now()
	res := TaskResult{Name: t.Name, Host: t.Host}
	name, args := meshCommandArgs(x.Remote, t.Host, x.RemoteShell)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(t.Body) // body runs via remote shell, or directly when disabled
	cmd.Env = tio.Env
	cmd.Stdout = tio.Stdout
	cmd.Stderr = tio.Stderr
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.ExitCode, res.Err = exitCodeFromExecErr(err)
	if res.ExitCode == 0 {
		res.Status = StatusDone
	} else {
		res.Status = StatusFailed
	}
	return res
}

// meshCommandArgs builds the argv to run a body on host via a remote shell.
// remote and remoteShell are shell-split ("ssh -i key -p 2222", "bash -s");
// the body arrives on stdin so it is never exposed on a command line. A
// remoteShell value of "none" omits the shell argv for outposts that consume
// stdin themselves and do not have bash.
func meshCommandArgs(remote, host, remoteShell string) (string, []string) {
	parts := strings.Fields(remote)
	if len(parts) == 0 {
		parts = []string{"ssh"}
	}
	args := append([]string{}, parts[1:]...)
	args = append(args, host)
	shell := strings.TrimSpace(remoteShell)
	if shell == "" {
		shell = "bash -s"
	}
	if !strings.EqualFold(shell, "none") {
		args = append(args, strings.Fields(shell)...)
	}
	return parts[0], args
}
