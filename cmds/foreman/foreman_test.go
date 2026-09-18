package foremancmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/foreman"
)

type stubRunner struct {
	prompts []string
	args    [][]string
	out     string
}

func (s *stubRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	s.args = append(s.args, append([]string(nil), args...))
	if len(args) > 0 {
		s.prompts = append(s.prompts, args[len(args)-1])
	}
	return s.out, 0, nil
}

func TestOnceYoloUsesOneCanonicalCodexBypassFlag(t *testing.T) {
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	old := runner
	r := &stubRunner{out: "done"}
	runner = r
	t.Cleanup(func() { runner = old })
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
	code := run(rc, []string{"--once", "--agent", "codex", "--instruction", "hello", "--yolo"})
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if len(r.args) != 1 {
		t.Fatalf("launches = %d, want 1", len(r.args))
	}
	count := 0
	for _, arg := range r.args[0] {
		if arg == "--dangerously-bypass-approvals-and-sandbox" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("codex bypass count = %d, argv = %q", count, r.args[0])
	}
}

func TestRegistered(t *testing.T) {
	if tool.Lookup("foreman") == nil {
		t.Fatal("foreman is not registered")
	}
}

func TestOnceUsesChatInvokePath(t *testing.T) {
	old := runner
	r := &stubRunner{out: "done"}
	runner = r
	t.Cleanup(func() { runner = old })
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
	code := run(rc, []string{"--once", "--agent", "stub", "--instruction", "hello"})
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "done" {
		t.Fatalf("out = %q, want done", got)
	}
	if len(r.prompts) != 1 || !strings.Contains(r.prompts[0], "hello") {
		t.Fatalf("prompts = %#v, want hello", r.prompts)
	}
}

func TestStartDetachStatusRoundTrip(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	t.Setenv("BASHY_FOREMAN_NO_SPAWN", "1")
	old := runner
	runner = &stubRunner{out: "ack"}
	t.Cleanup(func() { runner = old })
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
	code := run(rc, []string{"start", "--id", "cli", "--detach", "--goal", "round trip"})
	if code != 0 {
		t.Fatalf("start code = %d, err = %s", code, errb.String())
	}
	out.Reset()
	code = run(rc, []string{"status", "cli"})
	if code != 0 {
		t.Fatalf("status code = %d, err = %s", code, errb.String())
	}
	if got := out.String(); !strings.Contains(got, "cli\tidle\tround trip") {
		t.Fatalf("status = %q", got)
	}
	st, err := foreman.NewStore("", "cli").LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.MaxRuntime != "30m0s" || st.Deadline.IsZero() {
		t.Fatalf("runtime state = %+v", st)
	}
}

func TestStartRejectsInvalidMaxRuntime(t *testing.T) {
	for _, value := range []string{"0", "-1m", "forever"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
			var out, errb bytes.Buffer
			rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
			if code := run(rc, []string{"start", "--detach", "--goal", "bounded", "--max-runtime", value}); code != 1 {
				t.Fatalf("code = %d, err = %q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "positive duration") {
				t.Fatalf("err = %q", errb.String())
			}
		})
	}
}

func TestStartCanPersistUnboundedExactOnceSession(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	t.Setenv("BASHY_FOREMAN_NO_SPAWN", "1")
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
	code := run(rc, []string{"start", "--id", "owner", "--detach", "--goal", "manage", "--no-max-runtime", "--opening-send-once", "--yolo"})
	if code != 0 {
		t.Fatalf("start code = %d, err = %s", code, errb.String())
	}
	st, err := foreman.NewStore("", "owner").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.MaxRuntime != "" || !st.Deadline.IsZero() || !st.OpeningSendOnce || !st.AllowUnsafe {
		t.Fatalf("owner session policy was not persisted: %+v", st)
	}
	if code := run(rc, []string{"start", "--detach", "--goal", "bad", "--no-max-runtime", "--max-runtime", "1h"}); code == 0 {
		t.Fatal("conflicting runtime flags were accepted")
	}
}

func TestScriptedREPL(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	old := runner
	r := &stubRunner{out: "ack"}
	runner = r
	t.Cleanup(func() { runner = old })
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx: context.Background(),
		Dir: t.TempDir(),
		Stdio: tool.Stdio{
			In:  strings.NewReader("plain steering\nstatus\nstop\n"),
			Out: &out,
			Err: &errb,
		},
	}
	code := run(rc, []string{"run", "--id", "repl", "--goal", "scripted", "--agent", "stub"})
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if len(r.prompts) != 1 || !strings.Contains(r.prompts[0], "plain steering") {
		t.Fatalf("prompts = %#v, want plain steering", r.prompts)
	}
	if got := out.String(); !strings.Contains(got, "repl\tidle\tscripted") || !strings.Contains(got, "done") {
		t.Fatalf("repl output = %q", got)
	}
}

func TestRunDAGCommand(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	old := runner
	r := &stubRunner{out: "ack"}
	runner = r
	t.Cleanup(func() { runner = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.md")
	if err := os.WriteFile(path, []byte("## Tasks\n\n### a\n```bash\necho a\n```\n\n### b\nRequires: a\n```bash\necho b\n```\n"), 0o600); err != nil {
		t.Fatalf("write dag: %v", err)
	}
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Stdio: tool.Stdio{Out: &out, Err: &errb}}
	code := run(rc, []string{"run", "--agent", "stub", "dag.md"})
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "a\nb" {
		t.Fatalf("out = %q, want a/b", got)
	}
	if len(r.prompts) != 2 || !strings.Contains(r.prompts[0], "DAG target: a") || !strings.Contains(r.prompts[1], "DAG target: b") {
		t.Fatalf("prompts = %#v, want DAG targets", r.prompts)
	}
}

// `status --wait` is the state-change contract at the CLI: nothing on stdout
// for an unchanged session, exactly the missed transitions for a cursor, and
// NDJSON with no prose under --json.
func TestStatusWaitReturnsOnlyOnChange(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	s, err := foreman.Start(context.Background(), foreman.Options{ID: "w", Goal: "watchable", Agent: "stub", Runner: &stubRunner{out: "ack"}})
	if err != nil {
		t.Fatal(err)
	}
	newRC := func() (*tool.RunContext, *bytes.Buffer, *bytes.Buffer) {
		var out, errb bytes.Buffer
		return &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}, &out, &errb
	}

	// The snapshot carries the cursor.
	rc, out, errb := newRC()
	if code := run(rc, []string{"status", "--json", "w"}); code != 0 {
		t.Fatalf("status: code %d, err %s", code, errb.String())
	}
	var st foreman.State
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || st.Seq != 1 || st.Digest == "" {
		t.Fatalf("status --json = %q (%v), want seq 1 with digest", out.String(), err)
	}

	// Unchanged: a bounded wait is silent and successful.
	rc, out, errb = newRC()
	if code := run(rc, []string{"status", "--wait", "150ms", "w"}); code != 0 {
		t.Fatalf("wait: code %d, err %s", code, errb.String())
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("unchanged wait produced payload: out=%q err=%q", out.String(), errb.String())
	}

	// A cursor behind the head replays exactly what was missed, at once.
	rc, out, _ = newRC()
	if code := run(rc, []string{"status", "--after", "0", "--json", "w"}); code != 0 {
		t.Fatal("after 0")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var tr foreman.Transition
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &tr) != nil || tr.Seq != 1 || tr.Status != foreman.StatusIdle || tr.SchemaVersion != foreman.TransitionSchemaVersion {
		t.Fatalf("status --after 0 --json = %q", out.String())
	}

	// A change lands while a wait is parked: the wait returns it and nothing else.
	rc, out, errb = newRC()
	done := make(chan int, 1)
	go func() { done <- run(rc, []string{"status", "--wait", "5s", "w"}) }()
	time.Sleep(150 * time.Millisecond)
	if err := s.Apply(context.Background(), foreman.Command{Verb: foreman.CommandPause}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().SaveState(s.State()); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("wait: code %d, err %s", code, errb.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("status --wait did not return on the change")
	}
	if got := out.String(); !strings.Contains(got, "w\t2\tidle->blocked\t\tpaused by operator\n") {
		t.Fatalf("wait output = %q", got)
	}

	// --watch streams until the context ends, NDJSON under --json.
	ctx, cancel := context.WithCancel(context.Background())
	var wout, werr bytes.Buffer
	wrc := &tool.RunContext{Ctx: ctx, Dir: t.TempDir(), Stdio: tool.Stdio{Out: &wout, Err: &werr}}
	go func() { done <- run(wrc, []string{"status", "--watch", "--after", "1", "--json", "w"}) }()
	time.Sleep(150 * time.Millisecond)
	if err := s.Apply(context.Background(), foreman.Command{Verb: foreman.CommandResume}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().SaveState(s.State()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(wout.String(), "\n") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("watch: code %d, err %s", code, werr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("status --watch did not stop on context cancel")
	}
	wlines := strings.Split(strings.TrimSpace(wout.String()), "\n")
	if len(wlines) != 2 {
		t.Fatalf("watch streamed %d records, want 2 (blocked, idle): %q", len(wlines), wout.String())
	}
	for i, want := range []string{foreman.StatusBlocked, foreman.StatusIdle} {
		var tr foreman.Transition
		if err := json.Unmarshal([]byte(wlines[i]), &tr); err != nil || tr.Seq != int64(i+2) || tr.Status != want {
			t.Fatalf("watch record %d = %q (%v), want seq %d %s", i, wlines[i], err, i+2, want)
		}
	}
}

func TestStatusRejectsBadWaitFlags(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	for _, args := range [][]string{
		{"status", "--wait", "nope", "w"},
		{"status", "--wait", "-1s", "w"},
		{"status", "--after", "x", "w"},
	} {
		var out, errb bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
		if code := run(rc, args); code != 2 {
			t.Fatalf("%v: code %d, err %q", args, code, errb.String())
		}
	}
	// An unknown session is an error, never a silent wait.
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
	if code := run(rc, []string{"status", "--wait", "1s", "missing"}); code != 1 {
		t.Fatalf("missing session: code %d, err %q", code, errb.String())
	}
}
