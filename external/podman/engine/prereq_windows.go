//go:build windows

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.podman.io/common/pkg/config"
	"go.podman.io/podman/v6/pkg/machine/define"
)

const wslInstallHint = "run an elevated PowerShell and execute `wsl --install --no-distribution`, reboot if Windows asks, then rerun `bashy podman`"

var getenv = os.Getenv

func ensurePlatformMachinePrereqs(ctx context.Context) error {
	provider, err := selectedWindowsMachineProvider()
	if err != nil {
		return err
	}
	if provider == define.HyperVVirt {
		return nil
	}

	status, err := runWSL(ctx, "--status")
	if wslStatusLooksReady(status, err) {
		return nil
	}
	if !wslStatusLooksInstallable(status, err) {
		return fmt.Errorf("bashy podman: WSL2 status check failed: %w%s", err, formatWSLOutput(status))
	}

	installOut, installErr := runWSL(ctx, "--install", "--no-distribution")
	if installErr != nil && wslInstallLooksStarted(installOut) {
		return fmt.Errorf("bashy podman: WSL2 setup was started but Windows must reboot before containers can run%s\nreboot Windows, then rerun `bashy podman`", formatWSLOutput(installOut))
	}
	if installErr != nil {
		return fmt.Errorf("bashy podman: WSL2 is not ready and automatic setup failed: %w%s\n%s", installErr, formatWSLOutput(installOut), wslInstallHint)
	}

	status, err = runWSL(ctx, "--status")
	if wslStatusLooksReady(status, err) {
		return nil
	}
	return fmt.Errorf("bashy podman: WSL2 setup was started but is not ready yet%s\nreboot Windows if prompted, then rerun `bashy podman`", formatWSLOutput(status))
}

func runWSL(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", args...)
	cmd.Env = append(cmd.Environ(), "WSL_UTF8=1")
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, ctx.Err()
	}
	return out, err
}

func selectedWindowsMachineProvider() (define.VMType, error) {
	provider := ""
	if cfg, err := config.Default(); err == nil {
		provider = cfg.Machine.Provider
	}
	if override := strings.TrimSpace(getenv("CONTAINERS_MACHINE_PROVIDER")); override != "" {
		provider = override
	}
	vmType, err := define.ParseVMType(provider, define.WSLVirt)
	if err != nil {
		return define.UnknownVirt, fmt.Errorf("bashy podman: invalid Windows machine provider %q: %w", provider, err)
	}
	return vmType, nil
}

func wslStatusLooksReady(out []byte, err error) bool {
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return false
	}
	s := normalizeWSLOutput(out)
	return !looksLikeWSLUsage(s) &&
		!strings.Contains(s, "not installed") &&
		!strings.Contains(s, "kernel file is not found") &&
		!strings.Contains(s, "enable the virtual machine platform") &&
		!strings.Contains(s, "enable \"virtual machine platform\"") &&
		!strings.Contains(s, "enable the \"windows subsystem for linux\"")
}

func wslStatusLooksInstallable(out []byte, err error) bool {
	s := normalizeWSLOutput(out)
	return err != nil ||
		looksLikeWSLUsage(s) ||
		strings.Contains(s, "not installed") ||
		strings.Contains(s, "kernel file is not found") ||
		strings.Contains(s, "enable the virtual machine platform") ||
		strings.Contains(s, "enable \"virtual machine platform\"") ||
		strings.Contains(s, "enable the \"windows subsystem for linux\"")
}

func looksLikeWSLUsage(s string) bool {
	return strings.Contains(s, "usage:") &&
		strings.Contains(s, "wsl.exe") &&
		strings.Contains(s, "--install")
}

func formatWSLOutput(out []byte) string {
	s := strings.TrimSpace(normalizeWSLOutput(out))
	if s == "" {
		return ""
	}
	return "\n\nwsl output:\n" + s
}

func normalizeWSLOutput(out []byte) string {
	s := strings.ToLower(string(out))
	return strings.ReplaceAll(s, "\x00", "")
}

func wslInstallLooksStarted(out []byte) bool {
	s := normalizeWSLOutput(out)
	return strings.Contains(s, "virtual machine platform has been installed") ||
		strings.Contains(s, "windows subsystem for linux has been installed") ||
		strings.Contains(s, "reboot")
}
