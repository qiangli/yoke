package dag

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// A body reads the caller's stdin, as a make recipe does:
// `echo x | bashy dag t` with a body that runs `cat` prints x.
func TestBodyReadsCallerStdin(t *testing.T) {
	var out bytes.Buffer
	res := bashInterp{}.Run(context.Background(), &Task{Name: "t", Body: `read line; printf 'got:%s\n' "$line"`}, TaskIO{
		Dir: t.TempDir(), Env: os.Environ(), Stdin: strings.NewReader("hello\n"), Stdout: &out, Stderr: &out,
	})
	if res.ExitCode != 0 || out.String() != "got:hello\n" {
		t.Fatalf("exit=%d out=%q err=%v", res.ExitCode, out.String(), res.Err)
	}
	out.Reset()
	res = bashInterp{}.Run(context.Background(), &Task{Name: "t", Body: `if read line; then echo read; else echo eof; fi`}, TaskIO{
		Dir: t.TempDir(), Env: os.Environ(), Stdout: &out, Stderr: &out,
	})
	if res.ExitCode != 0 || out.String() != "eof\n" {
		t.Fatalf("nil stdin: exit=%d out=%q", res.ExitCode, out.String())
	}
}

// The run-journal tee writes the sink first and exposes it, so a body runner
// can tell that the sink is still the caller's terminal.
func TestJournalTeeUnwrapsToItsSink(t *testing.T) {
	var sink, journal bytes.Buffer
	tee := journalTee{&sink, &journal}
	if n, err := tee.Write([]byte("abc")); n != 3 || err != nil || sink.String() != "abc" || journal.String() != "abc" {
		t.Fatalf("write: n=%d err=%v sink=%q journal=%q", n, err, sink.String(), journal.String())
	}
	if tee.Unwrap() != &sink {
		t.Fatal("Unwrap must return the sink")
	}
}
