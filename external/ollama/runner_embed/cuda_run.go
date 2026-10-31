package runner_embed

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CUDAStartWindowEnv overrides how long a CUDA runner has to report a CUDA
// device in its startup log (a Go duration, e.g. 90s).
const CUDAStartWindowEnv = "BASHY_OLLAMA_CUDA_START_WINDOW"

// DefaultCUDAStartWindow covers loading the ~1.8 GB backend and the first
// device query from the server.
const DefaultCUDAStartWindow = 60 * time.Second

// Startup-log evidence. ggml prints the device count when the CUDA backend
// initializes; the ollama engine's "system" line lists CUDA.0.* features only
// when device 0 is a loaded CUDA backend.
var (
	cudaFoundRe  = regexp.MustCompile(`ggml_cuda_init: found ([0-9]+) CUDA devices`)
	cudaSystemRe = regexp.MustCompile(`msg=system\b.*\bCUDA\.0\.`)
	cudaFailedRe = regexp.MustCompile(`ggml_cuda_init: failed to initialize|no CUDA-capable device`)
)

// cudaLogVerdict reports what one log line proves: (true, nil) a CUDA device,
// (false, err) a CUDA refusal, (false, nil) nothing.
func cudaLogVerdict(line string) (bool, error) {
	if m := cudaFoundRe.FindStringSubmatch(line); m != nil {
		if m[1] == "0" {
			return false, errors.New("the runner found 0 CUDA devices")
		}
		return true, nil
	}
	if cudaSystemRe.MatchString(line) {
		return true, nil
	}
	if cudaFailedRe.MatchString(line) {
		return false, fmt.Errorf("the runner could not initialize CUDA: %s", strings.TrimSpace(line))
	}
	return false, nil
}

// CUDAStartWindow resolves the start window from getenv.
func CUDAStartWindow(getenv func(string) string) time.Duration {
	if v := strings.TrimSpace(getenv(CUDAStartWindowEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultCUDAStartWindow
}

// RunCUDA runs the CUDA runner with args, relaying its output, and fails
// closed AFTER start: unless its log reports a CUDA device within window, the
// runner is stopped before it can serve and RunCUDA returns non-zero with a
// message naming why. Upstream ggml would otherwise answer on CPU.
func RunCUDA(ctx context.Context, l CUDALaunch, args []string, stdin io.Reader, stdout, stderr io.Writer, window time.Duration) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.Runner, args...)
	cmd.Env = l.Env(os.Environ())
	cmd.Stdin = stdin
	setDeathSignal(cmd)
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return refuseCUDA(stderr, err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return refuseCUDA(stderr, err)
	}
	if err := cmd.Start(); err != nil {
		return refuseCUDA(stderr, fmt.Errorf("start %s: %v", l.Runner, err))
	}

	verdict := make(chan error, 1)
	var once sync.Once
	report := func(err error) { once.Do(func() { verdict <- err }) }
	var wg sync.WaitGroup
	relay := func(r io.Reader, w io.Writer) {
		defer wg.Done()
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				_, _ = io.WriteString(w, line)
				if ok, bad := cudaLogVerdict(line); ok {
					report(nil)
				} else if bad != nil {
					report(bad)
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go relay(outPipe, stdout)
	go relay(errPipe, stderr)
	exited := make(chan struct{})
	go func() { wg.Wait(); close(exited) }()

	timer := time.NewTimer(window)
	defer timer.Stop()
	var refusal error
	select {
	case refusal = <-verdict:
	case <-timer.C:
		refusal = fmt.Errorf("the runner reported no CUDA device within %s (%s)", window, CUDAStartWindowEnv)
	case <-exited:
		select {
		case refusal = <-verdict:
		default:
			refusal = errors.New("the runner exited before reporting a CUDA device")
		}
	case <-ctx.Done():
		refusal = ctx.Err()
	}
	if refusal != nil {
		cancel()       // kills the runner: a CPU-backed runner must never serve
		_ = cmd.Wait() // closes the pipes even if a grandchild holds them
		<-exited
		return refuseCUDA(stderr, refusal)
	}
	<-exited
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func refuseCUDA(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "bashy ollama runner: CUDA selected but %v; refusing to serve on CPU (set %s=cpu to run on CPU explicitly)\n", err, BackendEnv)
	return 1
}

// DispatchCUDA is the runner re-exec hook for hosts (bashy's `runner`
// dispatch): when the selected backend is CUDA it ensures, preflights and
// supervises the CUDA runner and returns handled=true with the exit code.
// handled=false means "not a CUDA selection": run the embedded runner as
// before. A CUDA request that cannot be met is handled (non-zero), never
// passed on to the CPU runner.
func DispatchCUDA(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (handled bool, code int) {
	b, err := SelectBackend(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "bashy ollama runner: %v\n", err)
		return true, 1
	}
	if b != BackendCUDA {
		return false, 0
	}
	l, err := EnsureCUDA(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "bashy ollama runner: %v\n", err)
		return true, 1
	}
	return true, RunCUDA(ctx, l, args, stdin, stdout, stderr, CUDAStartWindow(os.Getenv))
}
