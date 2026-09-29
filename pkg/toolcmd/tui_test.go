package toolcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
)

// fakeTUI is a scripted session: it records every act in order and never
// touches a PTY, a socket or a process.
type fakeTUI struct {
	mu       sync.Mutex
	events   []string
	turn     string
	output   string
	live     bool
	closed   int
	sayErr   error
	blockCtx bool // WaitIdle blocks until ctx ends (a tool that never goes quiet)
	onSay    func(text string)
}

func (f *fakeTUI) rec(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, a...))
}

func (f *fakeTUI) Ready(ctx context.Context) error { f.rec("ready"); return nil }
func (f *fakeTUI) Say(text string) error {
	f.rec("say %s", text)
	if f.onSay != nil {
		f.onSay(text)
	}
	return f.sayErr
}
func (f *fakeTUI) Key(b []byte) error { f.rec("key %x", b); return nil }
func (f *fakeTUI) WaitIdle(ctx context.Context, quiet time.Duration) error {
	f.rec("wait %s", quiet)
	if f.blockCtx {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (f *fakeTUI) Turn() string   { f.rec("turn"); return f.turn }
func (f *fakeTUI) Output() string { return f.output }
func (f *fakeTUI) QuitLine(line string) error {
	f.rec("quit %q", line)
	f.mu.Lock()
	f.live = false
	f.mu.Unlock()
	return nil
}
func (f *fakeTUI) Live() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}
func (f *fakeTUI) Close() {
	f.rec("close")
	f.mu.Lock()
	f.closed++
	f.live = false
	f.mu.Unlock()
}

func (f *fakeTUI) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// withFake installs f as the session factory and zeroes the settle waits.
func withFake(t *testing.T, f *fakeTUI) *[]chat.SessionOptions {
	t.Helper()
	var got []chat.SessionOptions
	prevStart, prevSettle, prevGrace := startTUISession, tuiInterruptSettle, tuiQuitGrace
	startTUISession = func(ctx context.Context, agent string, opt chat.SessionOptions) (tuiSession, error) {
		got = append(got, opt)
		f.rec("start %s", agent)
		f.live = true
		return f, nil
	}
	tuiInterruptSettle, tuiQuitGrace = 0, 200*time.Millisecond
	t.Cleanup(func() { startTUISession, tuiInterruptSettle, tuiQuitGrace = prevStart, prevSettle, prevGrace })
	return &got
}

func tuiTool(interrupt, graceful bool) fleet.Tool {
	t := fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI}
	t.CLI.Launch.SteerExec = "claude --model {model}"
	t.CLI.Launch.SteerInterrupt = interrupt
	t.CLI.Launch.SupportsGracefulQuit = graceful
	return t
}

func planCmd() fleet.ToolCommand {
	return fleet.ToolCommand{
		Name: "plan", Slash: "/plan {args}", Mode: fleet.ToolCommandTUI, Quit: "/exit",
		Steps: []fleet.ToolCommandStep{{WaitIdle: "30s"}, {Say: "1"}, {Key: "esc"}},
	}
}

func TestTUIKeyBytes(t *testing.T) {
	cases := map[string][]byte{
		"esc": {0x1b}, "Escape": {0x1b}, "enter": {'\r'}, "RETURN": {'\r'},
		"tab": {'\t'}, "shift-tab": []byte("\x1b[Z"), "space": {' '},
		"backspace": {0x7f}, "ctrl-c": {0x03}, "ctrl-d": {0x04}, "ctrl-z": {0x1a},
		"up": []byte("\x1b[A"), "down": []byte("\x1b[B"), "right": []byte("\x1b[C"), "left": []byte("\x1b[D"),
		" esc ": {0x1b},
	}
	for name, want := range cases {
		got, err := keyBytes(name)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("keyBytes(%q) = %x, %v; want %x", name, got, err, want)
		}
	}
	for _, bad := range []string{"", "f13", "ctrl-", "ctrl-1", "meta-x"} {
		if _, err := keyBytes(bad); err == nil {
			t.Errorf("keyBytes(%q) accepted; want an error", bad)
		}
	}
}

func TestRunTUIStepOrderAndTurn(t *testing.T) {
	f := &fakeTUI{turn: "\x1b[1mPlan:\x1b[0m print hello"}
	opts := withFake(t, f)
	res, err := Run(context.Background(), tuiTool(false, true), planCmd(), "print hello", Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"start claude", "ready",
		"turn", "say /plan print hello", // mark, then the slash line
		"wait 30s",
		"turn", "say 1", // a say step: mark, then type (no ESC: tool does not declare steer_interrupt)
		"key 1b",
		"wait " + tuiFinalQuiet.String(), // last step was not a wait: settle before capture
		"turn",
		`quit "/exit"`,
		"close",
	}
	if got := f.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %q\nwant %q", got, want)
	}
	if res.Outcome != OutcomeSuccess || res.Text != "Plan: print hello" || res.Slash != "/plan print hello" {
		t.Fatalf("result = %+v", res)
	}
	if len(*opts) != 1 || (*opts)[0].Prompt != "" || (*opts)[0].Cwd == "" {
		t.Fatalf("session options = %+v (want empty opening prompt, a cwd)", *opts)
	}
}

func TestRunTUISlashWithoutArgsTokenAppends(t *testing.T) {
	f := &fakeTUI{}
	withFake(t, f)
	cmd := fleet.ToolCommand{Name: "plan", Slash: "/plan", Mode: fleet.ToolCommandTUI, Steps: []fleet.ToolCommandStep{{WaitIdle: "1s"}}}
	if _, err := Run(context.Background(), tuiTool(false, true), cmd, "do x", Options{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !slicesContain(f.log(), "say /plan do x") {
		t.Fatalf("events %q: want the caller text appended after a space", f.log())
	}
	// last step was a wait: no extra settle before the capture
	if slicesContain(f.log(), "wait "+tuiFinalQuiet.String()) {
		t.Fatalf("events %q: unexpected final settle after a trailing wait_idle", f.log())
	}
}

func TestRunTUISteerInterruptIsInherited(t *testing.T) {
	f := &fakeTUI{}
	withFake(t, f)
	if _, err := Run(context.Background(), tuiTool(true, true), planCmd(), "x", Options{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	i := indexOf(log, "say 1")
	if i < 2 || log[i-2] != "key 1b" {
		t.Fatalf("events %q: want ESC before the say step on a steer_interrupt tool", log)
	}
	// the opening slash line goes into a fresh, idle session: no ESC before it
	j := indexOf(log, "say /plan x")
	if j < 1 || strings.HasPrefix(log[j-1], "key") || strings.HasPrefix(log[j-2], "key") {
		t.Fatalf("events %q: no ESC expected before the slash line", log)
	}
}

func TestRunTUIQuitHandling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		quit     string
		graceful bool
		want     string // "" = no quit line sent
	}{
		{"declared", "/exit", false, `quit "/exit"`},
		{"tool default", "", true, `quit ""`},
		{"none", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTUI{}
			withFake(t, f)
			cmd := fleet.ToolCommand{Name: "plan", Slash: "/plan", Mode: fleet.ToolCommandTUI, Quit: tc.quit}
			if _, err := Run(context.Background(), tuiTool(false, tc.graceful), cmd, "", Options{Dir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			log := f.log()
			hasQuit := false
			for _, e := range log {
				if strings.HasPrefix(e, "quit") {
					hasQuit = true
					if e != tc.want {
						t.Fatalf("quit event %q, want %q", e, tc.want)
					}
				}
			}
			if hasQuit != (tc.want != "") {
				t.Fatalf("events %q: quit sent=%v, want %v", log, hasQuit, tc.want != "")
			}
			if log[len(log)-1] != "close" || f.closed != 1 {
				t.Fatalf("events %q: want exactly one close, last", log)
			}
		})
	}
}

func TestRunTUITimeoutTearsDown(t *testing.T) {
	f := &fakeTUI{blockCtx: true}
	withFake(t, f)
	cmd := planCmd()
	start := time.Now()
	res, err := Run(context.Background(), tuiTool(false, true), cmd, "x", Options{Dir: t.TempDir(), Timeout: 150 * time.Millisecond})
	if err == nil || res.Outcome != OutcomeTimeout {
		t.Fatalf("Run = %+v, %v; want a timeout", res, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	if f.closed != 1 {
		t.Fatalf("events %q: want the session closed on timeout", f.log())
	}
	for _, e := range f.log() {
		if strings.HasPrefix(e, "quit") {
			t.Fatalf("events %q: no graceful quit into a timed-out session", f.log())
		}
	}
}

func TestRunTUICancelledTearsDown(t *testing.T) {
	f := &fakeTUI{blockCtx: true}
	withFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	res, err := Run(ctx, tuiTool(false, true), planCmd(), "x", Options{Dir: t.TempDir()})
	if err == nil || res.Outcome != OutcomeCancelled || f.closed != 1 {
		t.Fatalf("Run = %+v, %v, closed=%d; want cancelled + closed", res, err, f.closed)
	}
}

func TestRunTUIErrorTearsDown(t *testing.T) {
	f := &fakeTUI{sayErr: errors.New("socket gone")}
	withFake(t, f)
	res, err := Run(context.Background(), tuiTool(false, true), planCmd(), "x", Options{Dir: t.TempDir()})
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "socket gone") {
		t.Fatalf("Run = %+v, %v; want the say error", res, err)
	}
	if f.closed != 1 {
		t.Fatalf("events %q: want the session closed on error", f.log())
	}
}

func TestRunTUIStartErrorNoSession(t *testing.T) {
	prev := startTUISession
	t.Cleanup(func() { startTUISession = prev })
	startTUISession = func(ctx context.Context, agent string, opt chat.SessionOptions) (tuiSession, error) {
		return nil, errors.New("agent live")
	}
	res, err := Run(context.Background(), tuiTool(false, true), planCmd(), "x", Options{Dir: t.TempDir()})
	if err == nil || res.Outcome != OutcomeError {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunTUIUnknownKeyRefusedBeforeStart(t *testing.T) {
	f := &fakeTUI{}
	withFake(t, f)
	cmd := fleet.ToolCommand{Name: "plan", Slash: "/plan", Mode: fleet.ToolCommandTUI, Steps: []fleet.ToolCommandStep{{Key: "hyper-q"}}}
	res, err := Run(context.Background(), tuiTool(false, true), cmd, "", Options{Dir: t.TempDir()})
	if err == nil || res.Outcome != OutcomeError || len(f.log()) != 0 {
		t.Fatalf("Run = %+v, %v, events %q; want refusal with no session", res, err, f.log())
	}
}

func TestRunTUIDryRun(t *testing.T) {
	f := &fakeTUI{}
	withFake(t, f)
	res, err := Run(context.Background(), tuiTool(true, true), planCmd(), "print hello", Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"say: /plan print hello", "wait_idle: 30s", "key: esc (1b)", "say: 1", "key: esc (1b)", "wait_idle: " + tuiFinalQuiet.String(), "quit: /exit"}
	if res.Outcome != OutcomeDryRun || !reflect.DeepEqual(res.Steps, want) || len(f.log()) != 0 {
		t.Fatalf("dry run = %+v (events %q)\nwant steps %q", res, f.log(), want)
	}
}

func TestRunTUIOutputSelection(t *testing.T) {
	t.Run("transcript", func(t *testing.T) {
		f := &fakeTUI{turn: "last", output: "banner\n\x1b[2Jwhole session"}
		withFake(t, f)
		cmd := planCmd()
		cmd.Output = OutputTranscript
		res, err := Run(context.Background(), tuiTool(false, true), cmd, "x", Options{Dir: t.TempDir()})
		if err != nil || res.Text != "banner\nwhole session" {
			t.Fatalf("Run = %q, %v", res.Text, err)
		}
	})
	t.Run("file", func(t *testing.T) {
		dir, _ := filepath.EvalSymlinks(t.TempDir()) // the runner resolves the workdir
		f := &fakeTUI{turn: "wrote it"}
		f.onSay = func(text string) {
			if strings.HasPrefix(text, "/plan") {
				_ = os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# plan"), 0o600)
				_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o600)
			}
		}
		withFake(t, f)
		cmd := planCmd()
		cmd.Output = "file:*.md"
		res, err := Run(context.Background(), tuiTool(false, true), cmd, "x", Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res.Artifacts, []string{filepath.Join(dir, "plan.md")}) || res.Text != "wrote it" {
			t.Fatalf("Run = %+v", res)
		}
	})
	t.Run("temp workdir removed for turn output", func(t *testing.T) {
		f := &fakeTUI{turn: "ok"}
		opts := withFake(t, f)
		res, err := Run(context.Background(), tuiTool(false, true), planCmd(), "x", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Dir == "" || (*opts)[0].Cwd != res.Dir {
			t.Fatalf("dir %q, session cwd %q", res.Dir, (*opts)[0].Cwd)
		}
		if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
			t.Fatalf("temp workdir %s left behind (%v)", res.Dir, err)
		}
	})
}

func TestRunTUIAgentAndModel(t *testing.T) {
	f := &fakeTUI{}
	withFake(t, f)
	if _, err := Run(context.Background(), tuiTool(false, true), planCmd(), "x", Options{Dir: t.TempDir(), Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if f.log()[0] != "start claude:sonnet" {
		t.Fatalf("events %q: want the tool:model binding", f.log())
	}
}

func slicesContain(s []string, v string) bool { return indexOf(s, v) >= 0 }

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return -1
}
