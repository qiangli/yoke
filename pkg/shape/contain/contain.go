// Package contain runs a shaping candidate command inside an isolated clone
// directory: cwd-only writes, no reads outside the clone and a system
// allowlist, a scrubbed environment, loopback-only network, an @guard effect
// cap, and procguard plus resource limits. When a requested level cannot be
// provided on the host it refuses with a typed error; it never downgrades.
package contain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// LevelProcess: procguard, scrubbed env, effect cap, resource limits. It
	// does NOT confine file access or network; it never claims to.
	LevelProcess Level = "process"
	// LevelSandbox: LevelProcess plus kernel-enforced cwd-only writes, no
	// reads outside the clone and system allowlist, loopback-only network.
	LevelSandbox Level = "sandbox"
)

// ErrUnavailable is matched (errors.Is) by every refusal for a containment
// level the host cannot provide. Activation must block on it.
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
	// Level defaults to LevelSandbox.
	Level Level
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
	env := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   dir,
		"TMPDIR": dir,
	}
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

// Run executes the spec under the requested containment.
func Run(ctx context.Context, s Spec) (Result, error) {
	if s.Level == "" {
		s.Level = LevelSandbox
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
	argv := s.Argv
	if s.Level == LevelSandbox {
		argv, err = sandboxArgv(dir, s.Argv)
		if err != nil {
			return res, err
		}
	}
	path, err := resolveOnPath(argv[0])
	if err != nil {
		return res, fmt.Errorf("contain: %w", err)
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = Scrub(dir, s.EnvAllow, s.EnvSet)
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
