package watchcmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   t.TempDir(),
		Env:   []string{"HOSTNAME=testhost"},
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestWatchLoopHeadersAndCycles(t *testing.T) {
	var out bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Env:   []string{"HOSTNAME=h"},
		Stdio: tool.Stdio{Out: &out},
	}
	calls := 0
	opts := watchOptions{
		interval:  1500 * time.Millisecond,
		argv:      []string{"echo", "ok"},
		maxCycles: 2,
		now:       func() time.Time { return time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC) },
		sleep:     func(context.Context, time.Duration) bool { return true },
		runCommand: func(context.Context, *tool.RunContext, watchOptions) ([]byte, int, error) {
			calls++
			return []byte("ok\n"), 0, nil
		},
	}
	if code := watchLoop(rc, opts); code != 0 {
		t.Fatalf("code=%d, want 0", code)
	}
	got := out.String()
	if calls != 2 || strings.Count(got, "Every 1.5s: echo ok    h: Sat Feb  3 04:05:06 2024") != 2 || strings.Count(got, "ok\n") != 2 {
		t.Fatalf("calls=%d out=%q", calls, got)
	}
}

func TestWatchDiffAndChangeExit(t *testing.T) {
	var out bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &out}}
	outputs := [][]byte{[]byte("abc\n"), []byte("axc\n")}
	opts := watchOptions{
		noTitle:   true,
		diff:      true,
		chgexit:   true,
		argv:      []string{"cmd"},
		maxCycles: 3,
		sleep:     func(context.Context, time.Duration) bool { return true },
		runCommand: func(context.Context, *tool.RunContext, watchOptions) ([]byte, int, error) {
			o := outputs[0]
			outputs = outputs[1:]
			return o, 0, nil
		},
	}
	if code := watchLoop(rc, opts); code != 0 {
		t.Fatalf("code=%d, want 0", code)
	}
	if got := out.String(); !strings.Contains(got, "a\x1b[7mx\x1b[0mc\n") {
		t.Fatalf("diff output = %q", got)
	}
}

func TestWatchErrexitAndCancel(t *testing.T) {
	rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &bytes.Buffer{}}}
	code := watchLoop(rc, watchOptions{
		noTitle:   true,
		errexit:   true,
		argv:      []string{"cmd"},
		maxCycles: 1,
		runCommand: func(context.Context, *tool.RunContext, watchOptions) ([]byte, int, error) {
			return []byte("bad\n"), 7, errors.New("exit status 7")
		},
	})
	if code != 8 {
		t.Fatalf("errexit code=%d, want 8", code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rc.Ctx = ctx
	code = watchLoop(rc, watchOptions{
		noTitle: true,
		argv:    []string{"cmd"},
		runCommand: func(context.Context, *tool.RunContext, watchOptions) ([]byte, int, error) {
			return []byte{}, 0, nil
		},
	})
	if code != 130 {
		t.Fatalf("cancel code=%d, want 130", code)
	}
}

func TestWatchRunErrors(t *testing.T) {
	_, errb, code := runTool(t)
	if code != 2 || !strings.Contains(errb, "missing command") {
		t.Fatalf("missing command = (%q, %d)", errb, code)
	}
	_, errb, code = runTool(t, "-n", "0", "echo")
	if code != 2 || !strings.Contains(errb, "interval") {
		t.Fatalf("bad interval = (%q, %d)", errb, code)
	}
}
