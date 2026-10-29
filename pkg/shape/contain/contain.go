// Package contain runs a shaping candidate command as ordinary Bash# under
// bashy's guards: a scrubbed, secret-free environment, an @guard effect cap,
// procguard, a timeout, and resource limits, with the clone directory as cwd.
// It is deliberately NOT a sandbox and needs none, on any OS.
//
// Isolation is something a unit asks for, never something shaping imposes:
// a unit that declares a sandbox island runs at LevelSandbox, which contain
// does not implement itself — it routes the run to bashy oci (the podman
// engine bashy provisions on Windows, macOS and Linux) through OCIRunner.
// When bashy oci cannot serve that request it refuses with a typed error;
// it never downgrades, and a failure there is bashy oci's bug to fix, not
// shaping's to work around.
package contain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
	"github.com/qiangli/yoke/pkg/procguard"
	"github.com/qiangli/yoke/pkg/resources"
)

// Level is the isolation strength a caller requires.
type Level string

const (
	// LevelProcess, the default: procguard, scrubbed env, effect cap, timeout
	// and resource limits — bashy's normal guards. It does NOT confine file
	// access or network; it never claims to.
	LevelProcess Level = "process"
	// LevelSandbox: an explicit isolation request, routed to bashy oci with
	// the clone as the only mount and no network. Only a unit (or operator)
	// that asks for it gets it.
	LevelSandbox Level = "sandbox"
)

// ErrUnavailable is matched (errors.Is) by every refusal for a containment
// level the host cannot provide. A run that asked for it must not proceed.
var ErrUnavailable = errors.New("contain: containment unavailable")

// UnavailableError is the typed refusal for an unsupported level.
type UnavailableError struct {
	Level  Level
	Reason string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("contain: level %q unavailable: %s", e.Level, e.Reason)
}
func (e *UnavailableError) Is(t error) bool { return t == ErrUnavailable }

// EffectError is the refusal when the command's effects exceed the cap.
type EffectError struct {
	Command  string
	Exceeded []string
}

func (e *EffectError) Error() string {
	return fmt.Sprintf("contain: %s exceeds effect cap: %s", e.Command, strings.Join(e.Exceeded, ","))
}

// Spec describes one contained run.
type Spec struct {
	// Dir is the isolated clone directory: the only writable location.
	Dir  string
	Argv []string
	// Level defaults to LevelProcess.
	Level Level
	// OCI routes a LevelSandbox run; nil = BashyOCI{}. Ignored otherwise.
	OCI OCIRunner
	// Cap is the @guard effect cap spec (e.g. "read,write"); required.
	Cap string
	// Effects declares the command's own effect atoms when argv[0] is not an
	// atlas tool — a trusted interpreter the host installed, say, rather than
	// a certified utility. It is only ever a substitute for the atlas lookup:
	// the declared atoms are still measured against Cap, and an empty value
	// keeps the atlas lookup (whose absence denies as "unknown"). Declaring
	// effects does NOT relax anything a caller gets for free; the caller is
	// asserting what it is about to run, and remains answerable for it.
	Effects []string
	// EnvAllow lists parent environment variable names the principal may
	// see; nothing else is inherited. EnvSet adds explicit values.
	EnvAllow []string
	EnvSet   map[string]string
	// Limits; zero values mean unlimited except Timeout, which defaults to 5m.
	MaxRSSBytes uint64
	MaxProcs    int
	Timeout     time.Duration
	Stdout      *os.File
	Stderr      *os.File
}

// Result is the outcome of a run.
type Result struct {
	ExitCode int
	Level    Level
	Limited  string // non-empty when a resource limit killed the run
}

// Scrub builds the child environment: a fixed minimal PATH/HOME/TMPDIR inside
// the clone, the allowlisted parent variables, then explicit values.
func Scrub(dir string, allow []string, set map[string]string) []string {
	env := scrubBase(runtime.GOOS, dir, os.Getenv)
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range set {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// scrubBase is the fixed, secret-free floor of a contained environment for
// goos. On Windows a process cannot start without SystemRoot, and HOME/TMPDIR
// are spelled USERPROFILE/APPDATA/LOCALAPPDATA/TEMP/TMP — all pointed inside
// the clone exactly as HOME and TMPDIR are, so the unit's state stays there.
// SystemRoot is the only value taken from the parent; it names the OS
// directory and carries nothing secret.
func scrubBase(goos, dir string, getenv func(string) string) map[string]string {
	env := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   dir,
		"TMPDIR": dir,
	}
	if goos != "windows" {
		return env
	}
	root := getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	env["SystemRoot"] = root
	env["PATH"] = root + `\System32;` + root
	env["PATHEXT"] = ".COM;.EXE;.BAT;.CMD"
	env["USERPROFILE"] = dir
	env["APPDATA"] = dir + `\AppData\Roaming`
	env["LOCALAPPDATA"] = dir + `\AppData\Local`
	env["TEMP"] = dir
	env["TMP"] = dir
	return env
}

// Run executes the spec under the requested containment.
func Run(ctx context.Context, s Spec) (Result, error) {
	if s.Level == "" {
		s.Level = LevelProcess
	}
	res := Result{Level: s.Level, ExitCode: -1}
	if len(s.Argv) == 0 || s.Dir == "" {
		return res, errors.New("contain: dir and argv are required")
	}
	if s.Level != LevelProcess && s.Level != LevelSandbox {
		return res, &UnavailableError{Level: s.Level, Reason: "unknown level"}
	}
	capv, err := advice.ParseCap(s.Cap)
	if err != nil {
		return res, fmt.Errorf("contain: %w", err)
	}
	name := filepath.Base(s.Argv[0])
	effects := s.Effects
	if len(effects) == 0 {
		if e, ok := atlas.Lookup(name); ok {
			effects = e.Effects
		}
	}
	if ex := capv.Exceeded(effects); len(ex) > 0 {
		return res, &EffectError{Command: name, Exceeded: ex}
	}
	dir, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return res, fmt.Errorf("contain: clone dir: %w", err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return res, errors.New("contain: clone dir is not a directory")
	}
	env := Scrub(dir, s.EnvAllow, s.EnvSet)
	var argv []string
	if s.Level == LevelSandbox {
		// The host never resolves the candidate's interpreter: it runs
		// inside the image, by name, where the image's PATH finds it.
		oci := s.OCI
		if oci == nil {
			oci = BashyOCI{}
		}
		argv, err = oci.Argv(dir, append([]string{filepath.Base(s.Argv[0])}, s.Argv[1:]...), env, s)
		if err != nil {
			return res, err
		}
		if len(argv) == 0 {
			return res, &UnavailableError{Level: s.Level, Reason: "oci runner produced no command"}
		}
		// The launcher is trusted host bashy: it sees what it needs to
		// find its engine, and hands the candidate only the scrubbed env.
		env = Scrub(dir, launcherEnv, nil)
	} else {
		runner, err := resolveOnPath(s.Argv[0])
		if err != nil {
			return res, fmt.Errorf("contain: %w", err)
		}
		runner, err = filepath.Abs(runner)
		if err != nil {
			return res, fmt.Errorf("contain: runner: %w", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(runner); rerr == nil {
			runner = resolved
		}
		argv = append([]string{runner}, s.Argv[1:]...)
	}
	path, err := resolveOnPath(argv[0])
	if err != nil {
		if s.Level == LevelSandbox {
			return res, &UnavailableError{Level: s.Level, Reason: "oci launcher: " + err.Error()}
		}
		return res, fmt.Errorf("contain: %w", err)
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	guard, err := procguard.Arm(cmd)
	if err != nil {
		return res, &UnavailableError{Level: s.Level, Reason: err.Error()}
	}
	startErr := cmd.Start()
	guard.Started(startErr)
	if startErr != nil {
		return res, startErr
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limited := make(chan string, 1)
	go watch(rctx, cmd.Process.Pid, s, limited)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-rctx.Done():
		kill(cmd.Process.Pid)
		waitErr = <-done
		if ctx.Err() == nil {
			res.Limited = "timeout"
		}
	}
	guard.Disarm()
	cancel()
	select {
	case l := <-limited:
		res.Limited = l
	default:
	}
	if waitErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, waitErr
}

func resolveOnPath(p string) (string, error) {
	if strings.ContainsRune(p, filepath.Separator) {
		return p, nil
	}
	return exec.LookPath(p)
}

func watch(ctx context.Context, pid int, s Spec, limited chan<- string) {
	if s.MaxRSSBytes == 0 && s.MaxProcs == 0 {
		return
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u, err := resources.ProcessTree(ctx, pid)
			if err != nil {
				continue
			}
			why := ""
			if s.MaxRSSBytes > 0 && u.RSSBytes > s.MaxRSSBytes {
				why = "memory"
			} else if s.MaxProcs > 0 && u.Processes > s.MaxProcs {
				why = "processes"
			}
			if why != "" {
				limited <- why
				for _, p := range u.PIDs {
					killPID(p)
				}
				kill(pid)
				return
			}
		}
	}
}
