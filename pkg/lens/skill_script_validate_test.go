package lens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidatePureOutputScript pins the standalone S22 validator: only simple
// literal calls to echo, printf, true, false and pwd pass; every other shape
// fails closed with an ExitInvalid error naming the construct and its line.
// Validation never executes anything.
func TestValidatePureOutputScript(t *testing.T) {
	for _, script := range []string{
		"echo hello\n",
		"echo\n",
		"printf '%s\n' hello\n",
		"printf '%d + %d = %d\n' 1 2 3\n",
		"true\n",
		"false\n",
		"pwd\n",
		"pwd -P\n",
		"echo a\necho b\n",
		"echo a; echo b\n",
		"echo \"hello world\" 'it works' a\\ b\n",
		"echo '$HOME stays literal'\n",
		"echo \"* stays literal when quoted\"\n",
		"# only a comment\n",
		"# leading comment\necho hi # trailing comment\n\n",
		"",
	} {
		if err := ValidatePureOutputScript([]byte(script)); err != nil {
			t.Errorf("%q: want valid, got %v", script, err)
		}
	}

	for _, c := range []struct {
		in   string
		want []string // every substring the error must contain
	}{
		{"echo hi > out.txt\n", []string{"redirection", "line 1"}},
		{"echo hi >> out.txt\n", []string{"redirection", "line 1"}},
		{"pwd < in.txt\n", []string{"redirection", "line 1"}},
		{"echo ok\necho hi > out.txt\n", []string{"redirection", "line 2"}},
		{"echo $(whoami)\n", []string{"command substitution", "line 1"}},
		{"echo `whoami`\n", []string{"command substitution", "line 1"}},
		{"echo $HOME\n", []string{"parameter expansion", "line 1"}},
		{"echo ok\necho ${X}\n", []string{"parameter expansion", "line 2"}},
		{"echo \"$HOME\"\n", []string{"parameter expansion", "line 1"}},
		{"echo $((1 + 2))\n", []string{"arithmetic", "line 1"}},
		{"echo *\n", []string{"glob", "line 1"}},
		{"echo foo?.txt\n", []string{"glob", "line 1"}},
		{"echo [abc]\n", []string{"glob", "line 1"}},
		{"echo a | pwd\n", []string{"pipeline", "line 1"}},
		{"true && echo hi\n", []string{"&&", "line 1"}},
		{"false || echo hi\n", []string{"||", "line 1"}},
		{"hi() { echo hi; }\n", []string{"function", "line 1"}},
		{"for i in 1 2; do echo $i; done\n", []string{"loop", "line 1"}},
		{"while true; do echo hi; done\n", []string{"loop", "line 1"}},
		{"until false; do echo hi; done\n", []string{"loop", "line 1"}},
		{"if true; then echo hi; fi\n", []string{"if", "line 1"}},
		{"case x in y) echo hi;; esac\n", []string{"case", "line 1"}},
		{"(echo hi)\n", []string{"subshell", "line 1"}},
		{"{ echo hi; }\n", []string{"block", "line 1"}},
		{"! true\n", []string{"negation", "line 1"}},
		{"echo hi &\n", []string{"background", "line 1"}},
		{"X=1\n", []string{"assignment", "line 1"}},
		{"X=1 echo hi\n", []string{"assignment", "line 1"}},
		{"rm -rf /\n", []string{"unknown command", `"rm"`, "line 1"}},
		{"ls\n", []string{"unknown command", "line 1"}},
		{"set -e\n", []string{"unknown command", "line 1"}},
		{"exit 0\n", []string{"unknown command", "line 1"}},
		{"echo ok\necho ok\nrm x\n", []string{"unknown command", "line 3"}},
		{"echo $X\n", []string{"parameter expansion", "line 1"}},
		{"echo \"unterminated\n", []string{"syntax error"}},
		{"if\n", []string{"syntax error"}},
	} {
		err := ValidatePureOutputScript([]byte(c.in))
		if err == nil {
			t.Errorf("%q: want error, got nil", c.in)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%q: error %q misses %q", c.in, err, w)
			}
		}
		wantExit(t, err, ExitInvalid)
	}

	// Validation never executes: a rejected script naming a sentinel file
	// must fail without creating it, and a valid script validates silently.
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := ValidatePureOutputScript([]byte("echo pwned > " + sentinel + "\n")); err == nil {
		t.Fatal("redirection script passed validation")
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatalf("validation executed the script: %v", statErr)
	}
	if err := ValidatePureOutputScript([]byte("echo harmless > " + sentinel + "\n")); err == nil {
		t.Fatal("redirection script passed validation")
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatalf("validation executed the script: %v", statErr)
	}

	// Deterministic under repeat calls.
	script := []byte("printf '%s\n' steady\n")
	if err := ValidatePureOutputScript(script); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePureOutputScript(script); err != nil {
		t.Fatal(err)
	}
	bad := []byte("echo $(date)\n")
	first := ValidatePureOutputScript(bad)
	second := ValidatePureOutputScript(bad)
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatal("validator is not deterministic")
	}
}
