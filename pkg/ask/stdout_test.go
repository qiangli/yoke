package ask

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// runAskCmd executes the ask command with captured streams while a helper
// goroutine plays the human: it waits for the request's answer channel to come
// up and delivers value. A nil value means nobody ever answers.
func runAskCmd(t *testing.T, args []string, value []byte) (execErr error, stdout, stderr string) {
	t.Helper()

	cmd := newAskCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)

	answered := make(chan error, 1)
	if value != nil {
		go func() {
			// waitFor cannot be used here: FailNow must not run outside the
			// test goroutine. Poll by hand and report through the channel.
			deadline := time.Now().Add(5 * time.Second)
			for {
				if time.Now().After(deadline) {
					answered <- errors.New("the request channel never came up")
					return
				}
				all, err := List()
				if err == nil && len(all) == 1 && channelReady(all[0].ID) {
					answered <- Answer(all[0], value)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	} else {
		close(answered)
	}

	execErr = cmd.Execute()
	if err := <-answered; err != nil {
		t.Fatalf("answering the request: %v", err)
	}
	return execErr, out.String(), errb.String()
}

// The --stdout contract: stdout carries the VALUE and nothing else. Every
// instruction, prompt, and rendezvous note belongs on stderr, because
// `bashy ask --stdout | bashy secret set NAME` stores whatever crosses stdout.
func TestStdoutEmitsOnlyTheValue(t *testing.T) {
	isolate(t)

	const secret = "ghp_pipe_me_and_nothing_else"
	err, stdout, stderr := runAskCmd(t,
		[]string{"--stdout", "--channel", "rendezvous", "--timeout", "10s"},
		[]byte(secret))
	if err != nil {
		t.Fatalf("ask --stdout failed: %v", err)
	}

	if stdout != secret+"\n" {
		t.Errorf("stdout must be exactly the value:\n got %q\nwant %q", stdout, secret+"\n")
	}
	// The instruction and sentinel must still reach the human — on stderr, and
	// specifically on the COMMAND's stderr, so a host that rewires the streams
	// keeps the guarantee.
	if !strings.Contains(stderr, "bashy ask claim") {
		t.Errorf("the rendezvous instruction is not on the command's stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, SchemaVersion) {
		t.Errorf("the %s sentinel is not on the command's stderr:\n%s", SchemaVersion, stderr)
	}
	if strings.Contains(stderr, secret) {
		t.Error("stderr leaked the value")
	}
}

// When no channel answers, --stdout must FAIL — non-zero, with NOTHING on
// stdout. The bug this pins: the rendezvous prose crossed stdout, and
// `| bashy secret set NAME` stored the instruction text as the secret.
func TestStdoutFailsWhenNoChannelAnswers(t *testing.T) {
	isolate(t)

	err, stdout, _ := runAskCmd(t,
		[]string{"--stdout", "--channel", "rendezvous", "--timeout", "300ms"},
		nil)
	if err == nil {
		t.Fatal("ask --stdout succeeded with no answer — a pipe would store garbage as a secret")
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when nobody answered, got %q", stdout)
	}
}
