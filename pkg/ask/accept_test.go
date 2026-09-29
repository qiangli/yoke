package ask

// Story #1243: `bashy ask` told an operator their working Z.ai key was NOT
// VALID; the same 49-byte value then authenticated with HTTP 200. ask must
// accept WHATEVER the human enters as long as it is non-empty after trimming
// the one trailing line terminator, and must never guess a provider's key
// format — there is no stable shape for "an API key", vendors change them
// without notice, and a false negative trains the operator to ignore the one
// warning that might someday be true. These tests pin that contract at the
// command surface, so any future shape-guessing on the value path turns them
// red before it reaches an operator.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// valueJudgements is the operator-visible vocabulary a verdict about the value
// would use. The accept tests refuse ALL of it, on both streams: if any hint
// about the value ever appears it must be purely informational, and nothing
// informational needs these words. ("warning" itself is not in the list — the
// sentinel line echoes the requester's argv, which under `go test -run` carries
// this test's own name.)
var valueJudgements = []string{"not valid", "invalid", "does not look", "malformed"}

func assertNoValueJudgement(t *testing.T, stdout, stderr string) {
	t.Helper()
	for _, w := range valueJudgements {
		for _, stream := range []struct{ name, text string }{{"stdout", stdout}, {"stderr", stderr}} {
			if strings.Contains(strings.ToLower(stream.text), w) {
				t.Errorf("%s passes judgement on the entered value (%q):\n%s", stream.name, w, stream.text)
			}
		}
	}
}

// The owner's exact scenario: a Z.ai-shaped key — 32 hex characters, a dot,
// 16 alphanumerics; 49 bytes — must be delivered untouched, with no
// invalid-key wording anywhere in the emitted output.
func TestAskAcceptsAZaiShapedKeyWithoutWarning(t *testing.T) {
	isolate(t)

	const key = "3f2a9c8e7b6d5a4f1e0c9b8a7d6e5f43.Ab3dEf6hIj9lMn0p"
	if len(key) != 49 {
		t.Fatalf("test key is %d bytes, want the reported 49", len(key))
	}

	err, stdout, stderr := runAskCmd(t,
		[]string{"--stdout", "--channel", "rendezvous", "--timeout", "10s", "--name", "ZAI_CODING_PLAN_KEY"},
		[]byte(key))
	if err != nil {
		t.Fatalf("ask refused a working key: %v", err)
	}
	if stdout != key+"\n" {
		t.Errorf("the delivered value is not the entered value:\n got %q\nwant %q", stdout, key+"\n")
	}
	assertNoValueJudgement(t, stdout, stderr)
}

// Unusual but legitimate values. Every one is somebody's real credential, and
// each has been rejected by some tool's well-meaning format check.
func TestAskAcceptsArbitraryNonEmptyValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"short", "x"},
		{"very-long", strings.Repeat("k9-", 1365) + "k"},
		{"punctuation", "p@$$w0rd!#%^&*(){}[]|\\;:'\",<>/?~`"},
		{"unicode", "café ☕ 密钥 🔑"},
		{"internal-spaces", "correct horse battery staple"},
		{"leading-and-trailing-space", "  padded  "},
		{"internal-newline", "line one\nline two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			err, stdout, stderr := runAskCmd(t,
				[]string{"--stdout", "--channel", "rendezvous", "--timeout", "10s"},
				[]byte(tc.value))
			if err != nil {
				t.Fatalf("ask refused a legitimate value: %v", err)
			}
			if stdout != tc.value+"\n" {
				t.Errorf("the delivered value is not the entered value:\n got %q\nwant %q", stdout, tc.value+"\n")
			}
			assertNoValueJudgement(t, stdout, stderr)
		})
	}
}

// Empty is the ONE refusal that stays: an empty answer is a decline wearing
// success's clothes, never a credential. Input that trimming the terminator
// empties counts as empty.
func TestAskRefusesEmptyInput(t *testing.T) {
	isolate(t)

	for _, stdin := range []string{"", "\n", "\r\n"} {
		r := newTestRequest(t)
		cmd := newAskCmd()
		var out, errb bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errb)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs([]string{"answer", r.ID})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("answer accepted empty input %q", stdin)
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("the refusal for %q does not say why: %v", stdin, err)
		}
		if hasValue(r.ID) {
			t.Errorf("an empty answer for %q was delivered anyway", stdin)
		}
	}

	// And at the API seam every channel funnels through.
	if err := Answer(newTestRequest(t), nil); err == nil {
		t.Fatal("Answer accepted an empty value")
	}
}

// The delivered value is byte-identical apart from ONE trailing line
// terminator — no other trimming, because silently eating a leading or
// trailing character from a credential is its own bug.
func TestAskTrimsOnlyTheTrailingNewline(t *testing.T) {
	cases := []struct{ name, stdin, want string }{
		{"one-newline", "v\n", "v"},
		{"crlf", "v\r\n", "v"},
		{"no-terminator", "v", "v"},
		{"two-newlines-keep-one", "v\n\n", "v\n"},
		{"trailing-space-kept", "v \n", "v "},
		{"trailing-tab-kept", "v\t\n", "v\t"},
		{"leading-space-kept", " v\n", " v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			err, stdout, _ := runAskAnsweredOnStdin(t, tc.stdin)
			if err != nil {
				t.Fatalf("ask failed: %v", err)
			}
			if stdout != tc.want+"\n" {
				t.Errorf("delivered value:\n got %q\nwant %q", stdout, tc.want+"\n")
			}
		})
	}
}

// runAskAnsweredOnStdin runs `ask --stdout` while a helper goroutine plays a
// harness delivering RAW stdin through `ask answer` — the path a real line
// terminator arrives on, unlike runAskCmd's direct Answer call.
func runAskAnsweredOnStdin(t *testing.T, stdin string) (execErr error, stdout, stderr string) {
	t.Helper()

	cmd := newAskCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--stdout", "--channel", "rendezvous", "--timeout", "10s"})

	answered := make(chan error, 1)
	go func() {
		// Poll by hand, as runAskCmd does: FailNow must not run off the test
		// goroutine.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if time.Now().After(deadline) {
				answered <- errors.New("the request channel never came up")
				return
			}
			all, err := List()
			if err == nil && len(all) == 1 && channelReady(all[0].ID) {
				ac := newAskCmd()
				var aout, aerrb bytes.Buffer
				ac.SetOut(&aout)
				ac.SetErr(&aerrb)
				ac.SetIn(strings.NewReader(stdin))
				ac.SetArgs([]string{"answer", all[0].ID})
				answered <- ac.Execute()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	execErr = cmd.Execute()
	if err := <-answered; err != nil {
		t.Fatalf("answering the request: %v", err)
	}
	return execErr, out.String(), errb.String()
}
