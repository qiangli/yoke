package secrets

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A PIPE must keep working exactly as before — that is the scripted path
// (`printf %s "$k" | bashy secret set NAME`) and the reason the command reads stdin
// at all: it keeps the key out of shell history.
//
// The bug was never the read. It was that at a TERMINAL the command asked for
// something without saying so: io.ReadAll(stdin) blocks with no prompt, forever, and
// is indistinguishable from a hang. It was reported as one.
func TestReadSecretValueFromAPipeIsUnchanged(t *testing.T) {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader("sk-the-key\n"))
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})

	got, err := readSecretValue(c, "ZAI_API_KEY")
	if err != nil {
		t.Fatalf("readSecretValue: %v", err)
	}
	if got != "sk-the-key" {
		t.Errorf("got %q, want %q (the trailing newline must be stripped)", got, "sk-the-key")
	}
}

// A non-TTY reader must NOT be prompted — otherwise the prompt lands in the middle of
// a pipeline's stderr, or worse, in a captured value.
func TestNoPromptWhenStdinIsNotATerminal(t *testing.T) {
	var errBuf bytes.Buffer
	c := &cobra.Command{}
	c.SetIn(strings.NewReader("sk-key"))
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&errBuf)

	if _, err := readSecretValue(c, "ZAI_API_KEY"); err != nil {
		t.Fatalf("readSecretValue: %v", err)
	}
	if errBuf.Len() != 0 {
		t.Errorf("a piped read printed a prompt to stderr: %q", errBuf.String())
	}
}

// Multi-line input (an rc-file paste, a key with a stray newline) reads to EOF and is
// right-trimmed — the previous behaviour, preserved.
func TestPipedValueIsRightTrimmedOnly(t *testing.T) {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader("  sk-with-leading-space\r\n"))
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})

	got, err := readSecretValue(c, "K")
	if err != nil {
		t.Fatalf("readSecretValue: %v", err)
	}
	// Leading whitespace is PRESERVED on the pipe path: a key is stored verbatim, and
	// silently mangling a credential is worse than storing an odd one.
	if got != "  sk-with-leading-space" {
		t.Errorf("got %q — the pipe path must right-trim the newline and touch nothing else", got)
	}
}
