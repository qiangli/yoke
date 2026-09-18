//go:build !windows

// Package procguard starts a command behind a process-group guard.
package procguard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const helperEnv = "BASHY_INTERNAL_PROCGUARD_V1"

type commandSpec struct {
	Path string
	Args []string
}

// Guard owns the parent-only write end that keeps the child-side guard asleep.
// Arm must be called before starting cmd; Started must be called with the start
// result, and Disarm immediately after cmd has been reaped. The guarantee is
// the command's inherited process group; see ContainsSessionEscapes.
type Guard struct {
	deathReader *os.File
	deathWriter *os.File
	specReader  *os.File
	specWriter  *os.File
	spec        []byte
}

// ContainsSessionEscapes reports whether this portable backend contains a
// descendant that deliberately leaves the guarded process group with setpgid
// or setsid. It does not: POSIX has no portable job object spanning sessions,
// and Darwin has no cgroup/PDEATHSIG equivalent. Linux cgroup containment can
// be added as a future backend without overstating this one.
func ContainsSessionEscapes() bool { return false }

// Arm rewrites cmd to re-exec the current binary in guard mode. The helper
// installs EOF and SIGHUP watchers before starting the requested command,
// eliminating the child-before-watcher window. The command specification is
// sent over a private pipe after Start, preserving Path and Args (including
// Args[0]); cwd, environment, stdio, and process attributes belong to the guard
// and are inherited unchanged by the requested command.
func Arm(cmd *exec.Cmd) (*Guard, error) {
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, fmt.Errorf("procguard: command is not initialized")
	}
	if len(cmd.ExtraFiles) != 0 {
		return nil, fmt.Errorf("procguard: guarded commands with inherited extra files are unsupported")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else if cmd.SysProcAttr.Pgid != 0 {
		return nil, fmt.Errorf("procguard: guarded command must lead its own process group")
	} else if !cmd.SysProcAttr.Setsid && !cmd.SysProcAttr.Setpgid {
		cmd.SysProcAttr.Setpgid = true
	}
	spec, err := json.Marshal(commandSpec{Path: cmd.Path, Args: append([]string(nil), cmd.Args...)})
	if err != nil {
		return nil, fmt.Errorf("procguard: encode command: %w", err)
	}
	deathReader, deathWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("procguard: parent-death pipe: %w", err)
	}
	specReader, specWriter, err := os.Pipe()
	if err != nil {
		_ = deathReader.Close()
		_ = deathWriter.Close()
		return nil, fmt.Errorf("procguard: command pipe: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		_ = deathReader.Close()
		_ = deathWriter.Close()
		_ = specReader.Close()
		_ = specWriter.Close()
		return nil, fmt.Errorf("procguard: resolve guard executable: %w", err)
	}
	cmd.Path = exe
	cmd.Args = []string{exe}
	cmd.Env = guardEnv(cmd.Env)
	cmd.ExtraFiles = []*os.File{deathReader, specReader}
	return &Guard{deathReader: deathReader, deathWriter: deathWriter, specReader: specReader, specWriter: specWriter, spec: spec}, nil
}

func guardEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, helperEnv+"=") {
			continue
		}
		out = append(out, item)
	}
	return append(out, helperEnv+"=1")
}

// Started completes Arm after cmd.Start. On success it sends the exact command
// specification; the helper cannot start the requested command before this
// handoff. Any failure closes every descriptor and starts nothing.
func (g *Guard) Started(startErr error) {
	if g == nil {
		return
	}
	_ = g.deathReader.Close()
	_ = g.specReader.Close()
	if startErr == nil {
		_, _ = g.specWriter.Write(g.spec)
	}
	_ = g.specWriter.Close()
	if startErr != nil {
		_ = g.deathWriter.Close()
	}
}

// Disarm closes the parent-death trigger after the guarded command has been
// reaped. The guard is the reaped command itself; no watcher child remains.
func (g *Guard) Disarm() {
	if g == nil {
		return
	}
	_ = g.deathWriter.Close()
}

func init() {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	runHelper()
	os.Exit(127)
}

func runHelper() {
	_ = os.Unsetenv(helperEnv)
	death := os.NewFile(3, "procguard-parent-death")
	specFile := os.NewFile(4, "procguard-command")
	if death == nil || specFile == nil {
		os.Exit(127)
	}
	raw, err := io.ReadAll(specFile)
	_ = specFile.Close()
	if err != nil {
		os.Exit(127)
	}
	var spec commandSpec
	if json.Unmarshal(raw, &spec) != nil || spec.Path == "" || len(spec.Args) == 0 {
		os.Exit(127)
	}

	parentGone := make(chan struct{}, 1)
	go func() {
		var one [1]byte
		_, _ = death.Read(one[:])
		parentGone <- struct{}{}
	}()
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)

	child := &exec.Cmd{
		Path:   spec.Path,
		Args:   append([]string(nil), spec.Args...),
		Env:    os.Environ(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	select {
	case <-parentGone:
		os.Exit(137)
	case <-hangup:
		os.Exit(129)
	default:
	}
	if err := child.Start(); err != nil {
		os.Exit(127)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-parentGone:
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		os.Exit(137)
	case <-hangup:
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		os.Exit(129)
	case err := <-done:
		relayStatus(err)
	}
}

func relayStatus(err error) {
	if err == nil {
		os.Exit(0)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				sig := status.Signal()
				// Most reset signals terminate us immediately, preserving the raw
				// wait status for our parent. SIGPIPE is special in the Go runtime
				// and can remain ignored after Reset; the old unbounded select then
				// left the guard alive forever after its child was already reaped.
				// Keep exact signal relay where the kernel honors it, with the
				// conventional 128+signal status as a bounded fallback.
				time.AfterFunc(250*time.Millisecond, func() {
					os.Exit(128 + int(sig))
				})
				signal.Reset(sig)
				_ = syscall.Kill(os.Getpid(), sig)
				select {}
			}
			os.Exit(status.ExitStatus())
		}
	}
	os.Exit(127)
}
