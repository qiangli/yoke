package dag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/procguard"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type capacityChildInput struct {
	Task      *Task  `json:"task"`
	Workspace string `json:"workspace"`
}

func init() {
	if os.Getenv("BASHY_CAPACITY_BODY_CHILD") != "1" {
		return
	}
	_ = os.Unsetenv("BASHY_CAPACITY_BODY_CHILD")
	var in capacityChildInput
	if decodeCapacity(os.Stdin, &in) != nil || in.Task == nil || in.Workspace == "" {
		os.Exit(2)
	}
	tio := TaskIO{Dir: in.Workspace, Env: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr}
	var res TaskResult
	if capacityBoundedBuiltinBody(in.Task.Body) {
		res = runCapacityBounded(context.Background(), in.Task, tio)
	} else {
		res = (bashInterp{}).Run(context.Background(), in.Task, tio)
	}
	os.Exit(res.ExitCode)
}
func runCapacityOwnedTask(ctx context.Context, task *Task, tio TaskIO, onStart func(int) error) (TaskResult, bool) {
	started := time.Now()
	res := TaskResult{Name: task.Name, Status: StatusFailed, ExitCode: 1}
	executable, err := os.Executable()
	if err != nil {
		res.Err = err
		return res, true
	}
	payload, err := json.Marshal(capacityChildInput{Task: task, Workspace: tio.Dir})
	if err != nil {
		res.Err = err
		return res, true
	}
	cmd := exec.CommandContext(ctx, executable)
	cmd.Env = append(append([]string(nil), tio.Env...), "BASHY_CAPACITY_BODY_CHILD=1")
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Stdout = tio.Stdout
	cmd.Stderr = tio.Stderr
	cmd.WaitDelay = 2 * time.Second
	prepareCapacityProcess(cmd)
	guard, e := procguard.Arm(cmd)
	if e != nil {
		res.Err = e
		return res, true
	}
	defer guard.Disarm()
	err = cmd.Start()
	guard.Started(err)
	if err == nil {
		if e := onStart(cmd.Process.Pid); e != nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
			res.Err = e
			return res, capacityProcessGone(cmd)
		}
		err = cmd.Wait()
	}
	res.ExitCode, res.Err = exitCodeFromExecErr(err)
	res.Duration = time.Since(started)
	if ctx.Err() != nil {
		res.Err = ctx.Err()
	}
	if res.ExitCode == 0 && res.Err == nil {
		res.Status = StatusDone
	}
	// Portable process groups detect redirected background descendants, but cannot
	// prove an arbitrary external command never escaped the group. Such demand is
	// retained for explicit termination reconciliation, even after a direct exit.
	return res, cmd.Process == nil || (capacityProcessGone(cmd) && capacityBoundedBuiltinBody(task.Body))
}
func capacityBoundedBuiltinBody(body string) bool {
	file, e := syntax.NewParser().Parse(strings.NewReader(body), "")
	if e != nil {
		return false
	}
	safe := true
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if n.Background {
				safe = false
			}
		case *syntax.CallExpr:
			if len(n.Assigns) > 0 || len(n.Args) == 0 {
				safe = false
				break
			}
			name := n.Args[0].Lit()
			switch name {
			case "printf", "echo", "true", "false", "sleep", ":":
			default:
				safe = false
			}
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.FuncDecl, *syntax.Subshell, *syntax.ParamExp, *syntax.ArithmExp:
			safe = false
		}
		return safe
	})
	return safe
}

var errCapacityDescendantsUnverified = errors.New("capacity retained: descendant termination requires explicit reconciliation")

// The proved lane installs no external-command fallback. Even if the embedding
// binary has no coreutils registry, sleep remains a context-bound local timer.
func runCapacityBounded(ctx context.Context, task *Task, tio TaskIO) TaskResult {
	start := time.Now()
	res := TaskResult{Name: task.Name, Status: StatusFailed, ExitCode: 1}
	program, e := syntax.NewParser().Parse(strings.NewReader(task.Body), task.Name)
	if e != nil {
		res.Err = e
		return res
	}
	runner, e := interp.New(interp.Dir(tio.Dir), interp.Env(expand.ListEnviron(tio.Env...)), interp.StdIO(nil, tio.Stdout, tio.Stderr), interp.ExecHandler(func(ctx context.Context, args []string) error {
		if len(args) < 2 || args[0] != "sleep" {
			return errors.New("command outside constrained capacity executor")
		}
		total := time.Duration(0)
		for _, arg := range args[1:] {
			d, e := time.ParseDuration(arg)
			if e != nil {
				d, e = time.ParseDuration(arg + "s")
			}
			if e != nil || d < 0 || d > 10*time.Minute {
				return errors.New("invalid bounded sleep")
			}
			total += d
			if total > 10*time.Minute {
				return errors.New("bounded sleep too long")
			}
		}
		timer := time.NewTimer(total)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}))
	if e == nil {
		e = runner.Run(ctx, program)
	}
	res.ExitCode, res.Err = exitCodeFromErr(e)
	res.Duration = time.Since(start)
	if res.ExitCode == 0 && res.Err == nil {
		res.Status = StatusDone
	}
	return res
}

func capacityPlatformSupportsExecution(goos string) bool { return goos == "linux" || goos == "darwin" }
