//go:build !linux

package runner_embed

import "os/exec"

// setDeathSignal is Linux-only; the CUDA runner is published for linux/amd64.
func setDeathSignal(*exec.Cmd) {}
