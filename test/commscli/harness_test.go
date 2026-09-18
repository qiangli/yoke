package commscli

// The hermetic host every audit test runs against: fresh temp dirs for every
// store a verb can read or write (board, room, fleet, HOME), and subprocesses
// that see ONLY an environment composed here — never the operator's. That is
// what makes the audit repeatable by anyone: nothing leaks in from the machine
// it happens to run on, and nothing it does can touch a real board.

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type world struct {
	exe   string // the audit binary (this test binary, re-exec'd)
	bin   string // the ONLY PATH dir subprocesses see (stubs live here)
	home  string // empty HOME — nothing may depend on the operator's
	mb    string // BASHY_MB_DIR: the message board
	room  string // BASHY_ROOM_DIR: bus timeline + drain cursors
	fleet string // BASHY_FLEET_DIR: the local fleet store (tools/, agents/)
	work  string // default cwd: a plain non-repo temp dir
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("binary-level audit (subprocess per assertion); run without -short")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	base := t.TempDir()
	w := &world{
		exe:   exe,
		bin:   filepath.Join(base, "bin"),
		home:  filepath.Join(base, "home"),
		mb:    filepath.Join(base, "mb"),
		room:  filepath.Join(base, "room"),
		fleet: filepath.Join(base, "fleet"),
		work:  filepath.Join(base, "work"),
	}
	for _, d := range []string{w.bin, w.home, w.mb, w.room, w.work,
		filepath.Join(w.fleet, "tools"), filepath.Join(w.fleet, "agents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// env composes the subprocess environment from scratch. The parent's
// environment is deliberately NOT inherited: this process runs under an agent
// harness whose BASHY_* / identity variables would silently change what the
// verbs resolve, and an audit that only passes with them set proves nothing.
func (w *world) env(extra map[string]string) []string {
	vars := map[string]string{
		"PATH":            w.bin,
		"COMMSCLI_TOOL":   "1",
		"BASHY_MB_DIR":    w.mb,
		"BASHY_ROOM_DIR":  w.room,
		"BASHY_FLEET_DIR": w.fleet,
	}
	if runtime.GOOS == "windows" {
		vars["USERPROFILE"] = w.home
		vars["TEMP"], vars["TMP"] = w.home, w.home
		vars["USERNAME"] = "audit-login"
		// The bare minimum Windows needs to exec anything at all.
		for _, k := range []string{"SystemRoot", "SystemDrive", "ComSpec", "PATHEXT",
			"windir", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"} {
			if v := os.Getenv(k); v != "" {
				vars[k] = v
			}
		}
	} else {
		vars["HOME"] = w.home
		vars["TMPDIR"] = w.home
		vars["USER"], vars["LOGNAME"] = "audit-login", "audit-login"
	}
	for k, v := range extra {
		if v == "" {
			delete(vars, k)
			continue
		}
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// withGitOnPath appends the host git's directory to the audit PATH, for the
// weave tests (weaveRepoRoot shells out to git — the one system tool that
// part of the surface requires). Reports false when the host has no git.
func (w *world) withGitOnPath(t *testing.T, vars map[string]string) bool {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		return false
	}
	vars["PATH"] = w.bin + string(os.PathListSeparator) + filepath.Dir(git)
	return true
}

type result struct {
	out, err string
	code     int
	elapsed  time.Duration
}

// run executes the audit binary with the composed environment and returns
// what a caller observes: stdout, stderr, the exit code, and how long it
// took. A non-exit failure (binary missing, signal) fails the test.
func (w *world) run(t *testing.T, extra map[string]string, args ...string) result {
	t.Helper()
	cmd := w.command(extra, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	err := cmd.Run()
	res := result{out: out.String(), err: errOut.String(), elapsed: time.Since(start)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %v: %v", args, err)
		}
		res.code = ee.ExitCode()
	}
	return res
}

func (w *world) command(extra map[string]string, args ...string) *exec.Cmd {
	cmd := exec.Command(w.exe, args...)
	cmd.Env = w.env(extra)
	cmd.Dir = w.work
	return cmd
}

// running is an in-flight audit invocation, for the bounded-wait tests: the
// waiter must already be blocking when the wakeup lands.
type running struct {
	cmd      *exec.Cmd
	out, err bytes.Buffer
	start    time.Time
	done     chan error
}

func (w *world) start(t *testing.T, extra map[string]string, args ...string) *running {
	t.Helper()
	r := &running{cmd: w.command(extra, args...), done: make(chan error, 1)}
	r.cmd.Stdout, r.cmd.Stderr = &r.out, &r.err
	r.start = time.Now()
	if err := r.cmd.Start(); err != nil {
		t.Fatalf("starting %v: %v", args, err)
	}
	go func() { r.done <- r.cmd.Wait() }()
	return r
}

// wait collects the invocation's observable outcome, bounded so a wait bug
// can never hang the suite.
func (r *running) wait(t *testing.T, bound time.Duration) result {
	t.Helper()
	select {
	case err := <-r.done:
		res := result{out: r.out.String(), err: r.err.String(), elapsed: time.Since(r.start)}
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("waiting on subprocess: %v", err)
			}
			res.code = ee.ExitCode()
		}
		return res
	case <-time.After(bound):
		_ = r.cmd.Process.Kill()
		<-r.done
		t.Fatalf("subprocess still running after %v\nstdout: %s\nstderr: %s", bound, r.out.String(), r.err.String())
		return result{}
	}
}

// plantStub installs an executable named tool on the audit PATH by copying
// this test binary (TestMain recognises the basename and acts the part).
// A hardlink is tried first; temp dirs can sit on another volume, so a plain
// copy is the fallback.
func (w *world) plantStub(t *testing.T, tool string) {
	t.Helper()
	name := tool
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(w.bin, name)
	if err := os.Link(w.exe, dst); err == nil {
		return
	}
	src, err := os.Open(w.exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// plantFleetEntry writes one YAML entry into the temp fleet store.
func (w *world) plantFleetEntry(t *testing.T, noun, name, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.fleet, noun, name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// plantBoardCursor gives name a read cursor, the trace that makes it an
// OBSERVED reader: whois resolves it with source=observed, and mb send
// reports it queued rather than unverified.
func (w *world) plantBoardCursor(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(w.mb, "seen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// boardPostCount counts durable posts, so "nothing was written" is checkable.
func (w *world) boardPostCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.mb, "posts.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}
