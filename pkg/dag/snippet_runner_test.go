package dag

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/snippet"
)

func TestSnippetRunner(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		effects    []string
		exit       int
		refused    string
	}{
		{"json", `echo '{"ok":true}'`, []string{"pure"}, 0, ""},
		{"denied", `fetch https://example.invalid`, []string{"read"}, CapDeniedStatus, "fetch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := snippet.Run(context.Background(), snippet.Snippet{Body: tc.body, Effects: tc.effects}, snippet.IO{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if r.Exit != tc.exit {
				t.Fatalf("exit=%d stderr=%s", r.Exit, r.Stderr)
			}
			if tc.refused != "" && (len(r.Refused) != 1 || r.Refused[0] != tc.refused) {
				t.Fatalf("refused=%v stderr=%s", r.Refused, r.Stderr)
			}
			if tc.name == "json" && strings.TrimSpace(string(r.Stdout)) != `{"ok":true}` {
				t.Fatal(string(r.Stdout))
			}
		})
	}
}

func TestSnippetTimeoutAndLimit(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	_, err := snippet.Run(ctx, snippet.Snippet{Body: "sleep 2", Timeout: 50 * time.Millisecond}, snippet.IO{Dir: t.TempDir()})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("timeout err=%v elapsed=%v", err, time.Since(start))
	}
	r, err := snippet.Run(ctx, snippet.Snippet{Body: "echo 123456789"}, snippet.IO{Dir: t.TempDir(), MaxStdout: 3})
	if err == nil || string(r.Stdout) != "123" {
		t.Fatalf("result=%q err=%v", r.Stdout, err)
	}
}

func TestSnippetNestedFence(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	body := "~~~py as py\ndef main() -> str:\n    return 'nested'\n~~~\nvalue := py.main()\necho \"$value\""
	r, err := snippet.Run(context.Background(), snippet.Snippet{Body: body, Effects: []string{"pure", "exec"}}, snippet.IO{Dir: t.TempDir()})
	if err != nil || r.Exit != 0 || strings.TrimSpace(string(r.Stdout)) != "nested" {
		t.Fatalf("exit=%d stdout=%q stderr=%q err=%v", r.Exit, r.Stdout, r.Stderr, err)
	}
}

type snippetReplacementInterp struct{}

func (snippetReplacementInterp) Run(_ context.Context, _ *Task, tio TaskIO) TaskResult {
	_, _ = tio.Stdout.Write([]byte("replacement"))
	return TaskResult{ExitCode: 0}
}

func TestSnippetUsesCurrentInterpreter(t *testing.T) {
	original, err := interpFor("bsh")
	if err != nil {
		t.Fatal(err)
	}
	RegisterInterpreter("bsh", snippetReplacementInterp{})
	defer RegisterInterpreter("bsh", original)
	r, err := snippet.Run(context.Background(), snippet.Snippet{Body: "echo old"}, snippet.IO{Dir: t.TempDir()})
	if err != nil || string(r.Stdout) != "replacement" {
		t.Fatalf("stdout=%q err=%v", r.Stdout, err)
	}
}

func TestSnippetOnlyInheritsDeclaredCredential(t *testing.T) {
	t.Setenv("UNDECLARED_API_KEY", "hidden-test-value")
	r, err := snippet.Run(context.Background(), snippet.Snippet{Body: `echo "${UNDECLARED_API_KEY:-missing}"`}, snippet.IO{Dir: t.TempDir()})
	if err != nil || r.Exit != 0 || strings.TrimSpace(string(r.Stdout)) != "missing" {
		t.Fatalf("stdout=%q stderr=%q err=%v", r.Stdout, r.Stderr, err)
	}
	r, err = snippet.Run(context.Background(), snippet.Snippet{Body: `echo "$DECLARED_API_KEY"`, Env: map[string]string{"DECLARED_API_KEY": "env:UNDECLARED_API_KEY"}}, snippet.IO{Dir: t.TempDir()})
	if err != nil || r.Exit != 0 || strings.TrimSpace(string(r.Stdout)) != "hidden-test-value" {
		t.Fatalf("stdout=%q stderr=%q err=%v", r.Stdout, r.Stderr, err)
	}
}

func TestSnippetDefaultSecretGrant(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fixture-deepseek-value")
	r, err := snippet.Run(context.Background(), snippet.Snippet{Body: `echo "$PROBE_KEY"`, Env: map[string]string{"PROBE_KEY": "secret:deepseek"}}, snippet.IO{Dir: t.TempDir()})
	if err != nil || r.Exit != 0 || strings.TrimSpace(string(r.Stdout)) != "fixture-deepseek-value" {
		t.Fatalf("stdout=%q stderr=%q err=%v", r.Stdout, r.Stderr, err)
	}
}
