//go:build !windows

package gitscm

import "os/exec"

func configureGitCommand(*exec.Cmd) {}

func killGitProcessTree(int) {}
